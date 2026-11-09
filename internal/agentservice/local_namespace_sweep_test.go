package agentservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func stagingDirectories(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".oss-transfer-staging-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// A finished download leaves no staging directory beside its files, while a
// staging directory in use is kept.
func TestIdleStagingNamespacesAreRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	registry, db := openTempRegistry(t, path)
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetTempRegistry(registry)
	for _, key := range []string{"medium/a.bin", "medium/b.bin", "top.bin"} {
		if result, err := local.Write(context.Background(), key, strings.NewReader("value"), transferstore.WriteOptions{Size: 5, Overwrite: true}); err != nil || !result.Committed {
			t.Fatalf("write %s: %+v %v", key, result, err)
		}
	}
	if len(stagingDirectories(t, filepath.Join(root, "medium"))) != 1 || len(stagingDirectories(t, root)) != 1 {
		t.Fatal("fixture did not stage through namespaces")
	}
	// A writer still holding a namespace keeps it.
	held, err := os.OpenRoot(filepath.Join(root, "medium"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	namespace, err := registry.PrepareNamespace(context.Background(), held, filepath.Join(root, "medium"))
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := registry.SweepNamespaces(context.Background()); err != nil || removed != 1 {
		t.Fatalf("sweep with one namespace in use: %d %v", removed, err)
	}
	if len(stagingDirectories(t, filepath.Join(root, "medium"))) != 1 || len(stagingDirectories(t, root)) != 0 {
		t.Fatal("sweep removed a namespace in use or kept an idle one")
	}
	registry.ReleaseNamespace(namespace)
	if removed, err := registry.SweepNamespaces(context.Background()); err != nil || removed != 1 || len(stagingDirectories(t, filepath.Join(root, "medium"))) != 0 {
		t.Fatalf("released namespace was not removed: %d %v", removed, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "medium", "a.bin")); err != nil || string(data) != "value" {
		t.Fatalf("downloaded file changed: %q %v", data, err)
	}
	count := 0
	if err := db.View(func(tx *state.Tx) error {
		return tx.ScanKeys(localTempNamespacesBucket, "", "", 100, func(string, int64) error { count++; return nil })
	}); err != nil || count != 0 {
		t.Fatalf("namespace records kept: %d %v", count, err)
	}
	// Later writes stage through a fresh namespace.
	if result, err := local.Write(context.Background(), "medium/c.bin", strings.NewReader("value"), transferstore.WriteOptions{Size: 5, Overwrite: true}); err != nil || !result.Committed {
		t.Fatalf("write after sweep: %+v %v", result, err)
	}
}

// Staging directories left by an older Agent or a crash are removed at the
// next start once they hold nothing to recover.
func TestRecoveryRemovesEmptyStagingNamespaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	registry, db := openTempRegistry(t, path)
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	if _, err := local.Write(context.Background(), "dir/file", strings.NewReader("value"), transferstore.WriteOptions{Size: 5, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	local.Close()
	if len(stagingDirectories(t, filepath.Join(root, "dir"))) != 1 {
		t.Fatal("fixture did not stage through a namespace")
	}
	registry.Close()
	restarted, err := NewLocalTempRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if report, err := restarted.Recover(context.Background()); err != nil || len(report.Retained) != 0 {
		t.Fatalf("recovery: %+v %v", report, err)
	}
	if len(stagingDirectories(t, filepath.Join(root, "dir"))) != 0 {
		t.Fatal("recovery kept an empty staging directory")
	}
}
