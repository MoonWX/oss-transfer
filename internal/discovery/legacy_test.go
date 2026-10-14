package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestLegacyManifestUnionDeduplicatesAndKeepsMissingKeysAcrossRestart(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{true: "exact", false: "union"}[exact], func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "agent.db")
			f := &localFactory{}
			scanner, db := newScanner(t, dbPath, f, Options{MaxBatchObjects: 1})
			if err := db.Update(func(tx *state.Tx) error {
				return tx.Put("migration", "state", map[string]any{"pending": true, "migrationId": "old"})
			}); err != nil {
				t.Fatal(err)
			}
			size := int64(9)
			manifest := LegacyManifest{MigrationID: "old", ParentID: 1, Exact: exact}
			rows := []LegacyKey{{Key: "0000.txt", Size: &size, Successful: true}, {Key: "missing", Size: &size, Successful: true}}
			for range 2 {
				if err := ImportLegacyKeys(db, manifest, rows); err != nil {
					t.Fatal(err)
				}
			}
			manifest.Sealed = true
			if err := ImportLegacyKeys(db, manifest, nil); err != nil {
				t.Fatal(err)
			}
			header, err := LegacyManifestStatus(db, 1)
			if err != nil || header.Count != 2 {
				t.Fatal(header, err)
			}
			spec := localSpec(sourceFiles(t, 3))
			spec.LegacyManifestID = "old"
			if exact {
				// V4 immutable inventory execution did not apply time filters.
				spec.ModifiedFrom = time.Now().Add(time.Hour)
				spec.ModifiedUntil = spec.ModifiedFrom.Add(time.Hour)
			}
			if _, err := scanner.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			state := waitStatus(t, scanner, Completed)
			want := int64(4)
			if exact {
				want = 2
			}
			if state.TotalObjects != want {
				t.Fatal(state)
			}
			assertBatches(t, scanner, 1, int(want))
			foundMissing := false
			var after int64
			for {
				batch, ok, err := scanner.Batch(context.Background(), 1, 1, after)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				if candidate, ok := batch.Legacy["missing"]; ok {
					foundMissing = candidate.Missing && candidate.Successful
				}
				after = batch.Number
			}
			if !foundMissing {
				t.Fatal("missing historical key was silently dropped")
			}
			if exact && f.scans.Load() != 0 {
				t.Fatal("inventory expanded to directory scan")
			}
			if err := scanner.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			scanner, _ = newScanner(t, dbPath, f, Options{})
			if _, err := scanner.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			state = waitStatus(t, scanner, Completed)
			if state.TotalObjects != want {
				t.Fatal("restart duplicated manifest keys", state)
			}
		})
	}
}

func TestLegacySingleMissingObjectIsReported(t *testing.T) {
	ctx := context.Background()
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put("migration", "state", map[string]any{"pending": true, "migrationId": "old"})
	}); err != nil {
		t.Fatal(err)
	}
	manifest := LegacyManifest{MigrationID: "old", ParentID: 1, Sealed: true}
	if err := ImportLegacyKeys(db, manifest, []LegacyKey{{Key: "removed.txt", Successful: true}}); err != nil {
		t.Fatal(err)
	}
	spec := localSpec(t.TempDir())
	spec.SingleKey, spec.LegacyManifestID = "removed.txt", "old"
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	finished := waitStatus(t, scanner, Completed)
	batch, found, err := scanner.Batch(ctx, 1, 1, 0)
	if err != nil || !found || finished.TotalObjects != 1 || !batch.Legacy["removed.txt"].Missing {
		t.Fatal(finished, batch, found, err)
	}
}
