package agentservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// A removal that failed after deleting the marker or the directory leaves a
// record nobody holds. The next PrepareNamespace finishes that removal and
// creates a fresh namespace instead of handing out the broken one.
func TestLocalTempPrepareReplacesNamespaceBrokenByFailedRemoval(t *testing.T) {
	for _, damage := range []string{"marker", "directory"} {
		t.Run(damage, func(t *testing.T) {
			registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
			if _, err := registry.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			first := trackedLocal(t, registry, root)
			deferredWrite(t, first, "out/a", "first", true)
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			records := namespaceRecords(t, db)
			if len(records) != 1 {
				t.Fatalf("expected one idle namespace: %+v", records)
			}
			staging := filepath.Join(records[0].ParentPath, records[0].Name)
			target := filepath.Join(staging, ".oss-transfer-namespace")
			if damage == "directory" {
				target = staging
			}
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			second := trackedLocal(t, registry, root)
			deferredWrite(t, second, "out/b", "second", true)
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			now := namespaceRecords(t, db)
			if len(now) != 1 || now[0] == records[0] {
				t.Fatalf("broken namespace was handed out again: %+v", now)
			}
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Fatalf("broken namespace remains: %v", err)
			}
		})
	}
}

// writeAndClose runs one "job": a tracked Local writes keys and closes.
func writeAndClose(t *testing.T, registry *LocalTempRegistry, root string, keys ...string) {
	t.Helper()
	local := trackedLocal(t, registry, root)
	for _, key := range keys {
		deferredWrite(t, local, key, "payload of "+key, true)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
}

// Deleting the output directory and immediately rerunning must work, while
// the old namespace is still idle and after the idle sweep tried to remove it.
func TestLocalTempRerunAfterOutputDirectoryDeleted(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		name := "idle-namespace-left"
		if sweep {
			name = "swept-before-delete"
		}
		t.Run(name, func(t *testing.T) {
			registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
			if _, err := registry.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			root := filepath.Join(base, "dl")
			for run := range 3 {
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				writeAndClose(t, registry, root, "bench/d01/a", "bench/d01/b", "bench/d02/a")
				if sweep {
					registry.sweepIdleNamespaces(context.Background(), time.Now().Add(time.Hour))
				}
				for _, key := range []string{"bench/d01/a", "bench/d01/b", "bench/d02/a"} {
					if body, err := os.ReadFile(filepath.Join(root, key)); err != nil || string(body) != "payload of "+key {
						t.Fatalf("run %d: %s: %q %v", run, key, body, err)
					}
				}
				// rm -rf the output, as an operator does before a rerun.
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			}
			registry.sweepIdleNamespaces(context.Background(), time.Now().Add(time.Hour))
			if records := namespaceRecords(t, db); len(records) != 0 {
				t.Fatalf("stale namespace records remain: %+v", records)
			}
		})
	}
}

// Agents that ran the previous build may already hold namespace records whose
// directory was deleted or recreated. Both restart recovery and the next
// PrepareNamespace drop them (no temp record points into them) instead of
// refusing every write into the recreated directory.
func TestLocalTempStaleNamespaceRecordFromEarlierBuild(t *testing.T) {
	for _, repair := range []string{"recovery", "prepare"} {
		t.Run(repair, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.db")
			root := t.TempDir()
			db, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewLocalTempRegistry(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Leave a namespace record behind, as the earlier build did when
			// its removal failed, then recreate the directory.
			out := filepath.Join(root, "out")
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			parent, err := os.OpenRoot(out)
			if err != nil {
				t.Fatal(err)
			}
			stale, err := registry.PrepareNamespace(context.Background(), parent, out)
			parent.Close()
			if err != nil {
				t.Fatal(err)
			}
			registry.Close()
			db.Close()
			if err := os.RemoveAll(out); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}

			var restarted *LocalTempRegistry
			var restartedDB *state.Store
			if repair == "recovery" {
				restarted, restartedDB = openTempRegistry(t, path)
				report, err := restarted.Recover(context.Background())
				if err != nil || report.RemovedNamespaces != 1 || len(report.Retained) != 0 {
					t.Fatalf("recovery kept the stale record: %+v %v", report, err)
				}
			} else {
				// Recovery of a record whose directory still exists elsewhere
				// is skipped here by registering the stale record as live.
				restarted, restartedDB = openTempRegistry(t, path)
				restarted.ready = true
			}
			writeAndClose(t, restarted, root, "out/a")
			if body, err := os.ReadFile(filepath.Join(out, "a")); err != nil || string(body) != "payload of out/a" {
				t.Fatalf("write into the recreated directory failed: %q %v", body, err)
			}
			for _, record := range namespaceRecords(t, restartedDB) {
				if record == stale {
					t.Fatal("stale namespace record survived")
				}
			}
		})
	}
}
