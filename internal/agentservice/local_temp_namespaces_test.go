package agentservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func namespaceRecords(t *testing.T, db *state.Store) []transferstore.LocalTempNamespace {
	t.Helper()
	var records []transferstore.LocalTempNamespace
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(localTempNamespacesBucket, "", "", 100, func(_ string, raw json.RawMessage) error {
			var record transferstore.LocalTempNamespace
			err := json.Unmarshal(raw, &record)
			records = append(records, record)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

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

func trackedLocal(t *testing.T, registry *LocalTempRegistry, root string) *transferstore.Local {
	t.Helper()
	local, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	return local
}

func deferredWrite(t *testing.T, local *transferstore.Local, key, body string, overwrite bool) {
	t.Helper()
	result, err := local.Write(context.Background(), key, strings.NewReader(body), transferstore.WriteOptions{Size: int64(len(body)), Overwrite: overwrite, DeferDurability: true})
	if err != nil || !result.Committed {
		t.Fatalf("write %s: %+v %v", key, result, err)
	}
}

func TestLocalTempNamespaceRemovedAfterLastLocalCloses(t *testing.T) {
	registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	first, second := trackedLocal(t, registry, root), trackedLocal(t, registry, root)
	deferredWrite(t, first, "out/a", "first", true)
	deferredWrite(t, second, "out/b", "second", false)
	if names := stagingDirectories(t, filepath.Join(root, "out")); len(names) != 1 || len(namespaceRecords(t, db)) != 1 {
		t.Fatalf("both Locals must share one registered namespace: %v", names)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// The second Local still uses the namespace.
	if len(stagingDirectories(t, filepath.Join(root, "out"))) != 1 || len(namespaceRecords(t, db)) != 1 {
		t.Fatal("namespace removed while another Local holds it")
	}
	deferredWrite(t, second, "out/c", "third", true)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	// Released namespaces wait idle for reuse by the next job.
	idle := stagingDirectories(t, filepath.Join(root, "out"))
	if len(idle) != 1 {
		t.Fatalf("idle namespace missing: %v", idle)
	}
	reuse := trackedLocal(t, registry, root)
	deferredWrite(t, reuse, "out/r", "reused", true)
	if names := stagingDirectories(t, filepath.Join(root, "out")); len(names) != 1 || names[0] != idle[0] {
		t.Fatalf("idle namespace was not reused: %v %v", idle, names)
	}
	if err := reuse.Close(); err != nil {
		t.Fatal(err)
	}
	registry.sweepIdleNamespaces(context.Background(), time.Now().Add(time.Hour))
	if names := stagingDirectories(t, filepath.Join(root, "out")); len(names) != 0 {
		t.Fatalf("finished transfer left staging directories: %v", names)
	}
	if len(namespaceRecords(t, db)) != 0 || len(tempRecords(t, db)) != 0 {
		t.Fatal("finished transfer left registry records")
	}
	for key, want := range map[string]string{"out/a": "first", "out/b": "second", "out/c": "third", "out/r": "reused"} {
		if body, err := os.ReadFile(filepath.Join(root, key)); err != nil || string(body) != want {
			t.Fatalf("%s: %q %v", key, body, err)
		}
	}
	// A later transfer into the same directory creates a fresh namespace.
	third := trackedLocal(t, registry, root)
	deferredWrite(t, third, "out/d", "fourth", true)
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the registry removes idle namespaces without waiting.
	registry.Close()
	if len(stagingDirectories(t, filepath.Join(root, "out"))) != 0 || len(namespaceRecords(t, db)) != 0 {
		t.Fatal("recreated namespace was not removed")
	}
}

func TestLocalTempNamespaceKeptWhileItHoldsRecords(t *testing.T) {
	registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	namespace, err := registry.PrepareNamespace(context.Background(), parent, root)
	if err != nil {
		t.Fatal(err)
	}
	// A record whose cleanup did not finish still needs the namespace at restart.
	record := transferstore.LocalTempRecord{Owner: registry.OwnerID(), ID: strings.Repeat("e", 32), ParentPath: filepath.Join(root, namespace.Name), ParentIdentity: namespace.DirectoryIdentity, FileIdentity: "/1/2"}
	record.Name = ".oss-transfer-" + record.Owner + "-" + record.ID + ".tmp"
	if err := registry.Register(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReleaseNamespace(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	if len(stagingDirectories(t, root)) != 1 || len(namespaceRecords(t, db)) != 1 {
		t.Fatal("namespace removed while a temporary-file record points into it")
	}
	if err := registry.ReleaseNamespace(context.Background(), namespace); err == nil {
		t.Fatal("unbalanced release accepted")
	}
}

// Namespaces left by earlier versions, or by a crash during removal, are
// removed by the next recovery.
func TestLocalTempRecoveryRemovesIdleNamespaces(t *testing.T) {
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
	var namespaces []transferstore.LocalTempNamespace
	for _, dir := range []string{"left", "marker-removed", "directory-removed", "foreign-file"} {
		full := filepath.Join(root, dir)
		if err := os.Mkdir(full, 0o755); err != nil {
			t.Fatal(err)
		}
		parent, err := os.OpenRoot(full)
		if err != nil {
			t.Fatal(err)
		}
		namespace, err := registry.PrepareNamespace(context.Background(), parent, full)
		parent.Close()
		if err != nil {
			t.Fatal(err)
		}
		namespaces = append(namespaces, namespace)
	}
	registry.Close()
	db.Close()
	staging := func(i int) string { return filepath.Join(namespaces[i].ParentPath, namespaces[i].Name) }
	if err := os.Remove(filepath.Join(staging(1), ".oss-transfer-namespace")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(staging(2)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging(3), "unknown"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, restartedDB := openTempRegistry(t, path)
	report, err := restarted.Recover(context.Background())
	if err != nil || report.RemovedNamespaces != 3 {
		t.Fatalf("idle namespaces were not removed: %+v %v", report, err)
	}
	for i := range 3 {
		if _, err := os.Stat(staging(i)); !os.IsNotExist(err) {
			t.Fatalf("namespace %d remains: %v", i, err)
		}
	}
	// A namespace holding an unknown file is kept, and so is its record.
	if body, err := os.ReadFile(filepath.Join(staging(3), "unknown")); err != nil || string(body) != "x" {
		t.Fatalf("recovery touched an unknown file: %v", err)
	}
	if records := namespaceRecords(t, restartedDB); len(records) != 1 || records[0] != namespaces[3] {
		t.Fatalf("unexpected namespace records: %+v", records)
	}
}
