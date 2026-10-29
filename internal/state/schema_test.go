package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func readFormat(t *testing.T, db *Store) databaseFormat {
	t.Helper()
	var format databaseFormat
	if err := db.View(func(tx *Tx) error {
		found, err := tx.Get("metadata", "format", &format)
		if err == nil && !found {
			t.Fatal("missing format")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return format
}

func TestSchemaRequirementCommitsWithDataAndNeverLowersVersion(t *testing.T) {
	for _, scenario := range []struct {
		role    string
		version int
	}{{"master", SchemaResolvedSources}, {"agent", SchemaResolvedSources}, {"master", SchemaInitialPassword}, {"master", SchemaTaskLeases}, {"agent", SchemaTaskLeases}, {"agent", SchemaReleasedScanBatches}} {
		role, version := scenario.role, scenario.version
		t.Run(fmt.Sprintf("%s-schema%d", role, version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), role+".db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			if err = db.ClaimRole(role); err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != SchemaCompressedValues {
				t.Fatal("current reader did not establish compression fence")
			}
			// Exercise feature fences on a database written by an old reader.
			if err = db.Update(func(tx *Tx) error { return tx.Put("metadata", "format", databaseFormat{Role: role, SchemaVersion: 1}) }); err != nil {
				t.Fatal(err)
			}
			abort := errors.New("failed result commit")
			if err = db.Update(func(tx *Tx) error {
				if err := RequireSchema(tx, role, version); err != nil {
					return err
				}
				if err := tx.Put("results", "pin", map[string]string{"etag": "first"}); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != 1 {
				t.Fatal("aborted pin raised reader requirement")
			}
			if err = db.View(func(tx *Tx) error {
				var pin any
				found, err := tx.Get("results", "pin", &pin)
				if found {
					t.Fatal("aborted pin survived")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err = db.Update(func(tx *Tx) error {
				if err := RequireSchema(tx, role, version); err != nil {
					return err
				}
				return tx.Put("results", "pin", map[string]string{"etag": "first"})
			}); err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != version {
				t.Fatal("committed feature did not establish its reader requirement")
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.ClaimRole(role); err != nil {
				t.Fatal("current reader rejected supported schema", err)
			}
			if err = db.Update(func(tx *Tx) error { return RequireSchema(tx, role, 1) }); err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db) != (databaseFormat{Role: role, SchemaVersion: max(version, SchemaCompressedValues)}) {
				t.Fatal("reader requirement was lost")
			}
			if err = db.View(func(tx *Tx) error {
				var pin map[string]string
				found, err := tx.Get("results", "pin", &pin)
				if !found || pin["etag"] != "first" {
					t.Fatal("committed pin lost")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSchemaRequirementRejectsUnclaimedWrongAndFutureFormats(t *testing.T) {
	for _, scenario := range []string{"unclaimed", "wrong-role", "invalid-role", "zero", "future", "future-requirement"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			format := databaseFormat{Role: "agent", SchemaVersion: 1}
			switch scenario {
			case "wrong-role":
				format.Role = "master"
			case "zero":
				format.SchemaVersion = 0
			case "future":
				format.SchemaVersion = latestSchema + 1
			}
			if scenario != "unclaimed" {
				if err = db.Update(func(tx *Tx) error { return tx.Put("metadata", "format", format) }); err != nil {
					t.Fatal(err)
				}
			}
			role, version := "agent", SchemaResolvedSources
			if scenario == "invalid-role" {
				role = "unknown"
			}
			if scenario == "future-requirement" {
				version = latestSchema + 1
			}
			before, err := os.ReadFile(db.db.Path())
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Update(func(tx *Tx) error { return RequireSchema(tx, role, version) }); err == nil {
				t.Fatal("invalid format accepted")
			}
			after, err := os.ReadFile(db.db.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected requirement changed database")
			}
			if scenario == "wrong-role" || scenario == "zero" || scenario == "future" {
				if err = db.ClaimRole("agent"); err == nil {
					t.Fatal("ClaimRole accepted incompatible format")
				}
			}
		})
	}
}

func TestSchemaOfflineBackupAndRestore(t *testing.T) {
	for _, scenario := range []struct {
		role    string
		version int
	}{{"master", SchemaResolvedSources}, {"agent", SchemaResolvedSources}, {"master", SchemaInitialPassword}, {"master", SchemaTaskLeases}, {"agent", SchemaTaskLeases}, {"agent", SchemaReleasedScanBatches}} {
		role, version := scenario.role, scenario.version
		t.Run(fmt.Sprintf("%s-schema%d", role, version), func(t *testing.T) {
			path, oldBackup := restoreFixture(t, role)
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Update(func(tx *Tx) error {
				if err := RequireSchema(tx, role, version); err != nil {
					return err
				}
				return tx.Put("results", "pin", map[string]string{"etag": "first"})
			}); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			newBackup := filepath.Join(filepath.Dir(path), "pinned.db")
			if err = BackupOffline(path, role, newBackup); err != nil {
				t.Fatal(err)
			}
			if err = RestoreOffline(path, role, oldBackup); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != 1 {
				t.Fatal("old backup was not restored")
			}
			db.Close()
			if err = RestoreOffline(path, role, newBackup); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if readFormat(t, db).SchemaVersion != version {
				t.Fatal("restored backup lost its original reader requirement")
			}
			if err = db.ClaimRole(role); err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != max(version, SchemaCompressedValues) {
				t.Fatal("current reader lost compression fence")
			}
		})
	}
}
