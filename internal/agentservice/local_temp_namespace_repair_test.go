package agentservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
