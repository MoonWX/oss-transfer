package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTaskLeaseSchemaKeepsCompressionAndFencesAcrossMaintenance(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path, _ := restoreFixture(t, role)
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if err := db.ClaimRole(role); err != nil {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != SchemaCompressedValues {
				t.Fatal("ordinary databases should not require takeover support")
			}
			lease := map[string]any{"parentId": float64(1), "generation": float64(2), "required": true, "retired": true, "leaseId": "persisted-owner"}
			writeLease := func(tx *Tx) error {
				if err := RequireSchema(tx, role, SchemaTaskLeases); err != nil {
					return err
				}
				return tx.Put("leases", "1", lease)
			}
			aborted := errors.New("aborted lease")
			if err := db.Update(func(tx *Tx) error {
				if err := writeLease(tx); err != nil {
					return err
				}
				return aborted
			}); !errors.Is(err, aborted) {
				t.Fatal(err)
			}
			if readFormat(t, db).SchemaVersion != SchemaCompressedValues {
				t.Fatal("aborted lease raised schema")
			}
			if err := db.View(func(tx *Tx) error {
				var value any
				found, err := tx.Get("leases", "1", &value)
				if found {
					t.Fatal("aborted lease was persisted")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			value := strings.Repeat("same-training-object-metadata/", 1000)
			if err := db.Update(func(tx *Tx) error {
				if err := writeLease(tx); err != nil {
					return err
				}
				return tx.Put("records", "compressed", value)
			}); err != nil {
				t.Fatal(err)
			}
			verify := func(current *Store) {
				t.Helper()
				if err := current.View(func(tx *Tx) error {
					// Even a schema-4 reader can decode this uncompressed format row
					// and reject the database before touching task ownership records.
					var format databaseFormat
					if err := json.Unmarshal(tx.tx.Bucket([]byte("metadata")).Get([]byte("format")), &format); err != nil {
						return err
					}
					if format.Role != role || format.SchemaVersion != SchemaTaskLeases || format.SchemaVersion <= SchemaCompressedValues {
						t.Fatal("maintenance lost the old-reader ownership fence", format)
					}
					var saved map[string]any
					found, err := tx.Get("leases", "1", &saved)
					if err != nil {
						return err
					}
					if !found || !reflect.DeepEqual(saved, lease) {
						t.Fatal("lease/retirement state changed", saved)
					}
					stored := tx.tx.Bucket([]byte("records")).Get([]byte("compressed"))
					if !bytes.HasPrefix(stored, compressedMagic) || len(stored) >= len(value)/4 {
						t.Fatalf("lease schema disabled compression: %d/%d", len(stored), len(value))
					}
					var actual string
					found, err = tx.Get("records", "compressed", &actual)
					if err == nil && (!found || actual != value) {
						t.Fatal("compressed payload changed")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			verify(db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(filepath.Dir(path), "leases-backup.db")
			compact := filepath.Join(filepath.Dir(path), "leases-compact.db")
			if err := BackupOffline(path, role, backup); err != nil {
				t.Fatal(err)
			}
			before := fileBytes(t, path)
			if err := CompactOffline(path, role, compact); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("compaction changed source database")
			}
			for _, retained := range []string{backup, compact} {
				copy, err := Open(retained)
				if err != nil {
					t.Fatal(err)
				}
				verify(copy)
				if err := copy.Close(); err != nil {
					t.Fatal(err)
				}
				if err := RestoreOffline(path, role, retained); err != nil {
					t.Fatal(err)
				}
				db, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.ClaimRole(role); err != nil {
					t.Fatal(err)
				}
				// A later feature requiring only schema 4 must not remove the fence.
				if err := db.Update(func(tx *Tx) error { return RequireSchema(tx, role, SchemaCompressedValues) }); err != nil {
					t.Fatal(err)
				}
				verify(db)
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
