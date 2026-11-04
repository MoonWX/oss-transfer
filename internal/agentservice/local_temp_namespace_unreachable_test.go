package agentservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A namespace whose parent is only unreachable (recorded on another
// filesystem, like a volume that is not mounted now) is never forgotten: its
// record is kept for later recoveries, while the directory that is at that
// path now gets a fresh namespace, so writes keep working.
func TestLocalTempUnreachableNamespaceRecordIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	root := t.TempDir()
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
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
	parent, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	original, err := registry.PrepareNamespace(context.Background(), parent, out)
	parent.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Pretend the record was made on another filesystem mounted at out.
	unmounted := original
	unmounted.ParentIdentity = "/" + strings.Repeat("9", 12) + "/1"
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put(localTempNamespacesBucket, namespaceKey(out), unmounted)
	}); err != nil {
		t.Fatal(err)
	}
	registry.Close()
	db.Close()

	restarted, restartedDB := openTempRegistry(t, path)
	report, err := restarted.Recover(context.Background())
	if err != nil || report.RemovedNamespaces != 0 || len(report.Retained) != 1 {
		t.Fatalf("recovery dropped or ignored an unreachable namespace: %+v %v", report, err)
	}
	writeAndClose(t, restarted, root, "out/a")
	if body, err := os.ReadFile(filepath.Join(out, "a")); err != nil || string(body) != "payload of out/a" {
		t.Fatalf("write next to an unreachable namespace failed: %q %v", body, err)
	}
	keys := map[string]transferstore.LocalTempNamespace{}
	if err := restartedDB.View(func(tx *state.Tx) error {
		return tx.Scan(localTempNamespacesBucket, "", "", 100, func(key string, raw json.RawMessage) error {
			var record transferstore.LocalTempNamespace
			err := json.Unmarshal(raw, &record)
			keys[key] = record
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	if keys[retainedNamespaceKey(unmounted)] != unmounted {
		t.Fatalf("unreachable record was not kept under its own key: %+v", keys)
	}
	// Recovery keeps visiting it without error about the key layout.
	restarted.Close()
	restartedDB.Close()
	again, _ := openTempRegistry(t, path)
	if report, err := again.Recover(context.Background()); err != nil || len(report.Retained) != 1 {
		t.Fatalf("second recovery: %+v %v", report, err)
	}
}
