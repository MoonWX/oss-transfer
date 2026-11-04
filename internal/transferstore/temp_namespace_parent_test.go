package transferstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func directoryIdentity(t *testing.T, path string) string {
	t.Helper()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := storageIdentity(root, ".", info)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// Only positive evidence that the parent was deleted lets a namespace record
// go: the path resolving to another directory, or missing below an existing
// one, on the recorded filesystem. A missing path above an unmounted volume,
// or a path on another filesystem, keeps the record.
func TestNamespaceParentClassification(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "out")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	identity := directoryIdentity(t, parent)
	device := identityDevice(identity)
	owner, id := strings.Repeat("ab", 32), strings.Repeat("cd", 16)
	record := LocalTempNamespace{Owner: owner, ID: id, ParentPath: parent, ParentIdentity: identity, Name: namespaceName(owner, id), DirectoryIdentity: "/1/2"}
	otherDevice := "/" + device + "0/1"

	// Deleted and recreated on the same filesystem.
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// A filesystem may reuse the inode number; the namespace is missing
	// either way, which also counts as removed.
	if err := RemoveLocalTempNamespace(context.Background(), record); err != nil && !errors.Is(err, ErrTempNamespaceGone) {
		t.Fatalf("recreated parent: got %v", err)
	}
	// Deleted, with its parent still present on the same filesystem.
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLocalTempNamespace(context.Background(), record); !errors.Is(err, ErrTempNamespaceGone) {
		t.Fatalf("deleted parent: got %v", err)
	}
	// Recorded on another filesystem, like a volume that is not mounted now:
	// the path is missing, or resolves to the mount point's filesystem.
	unmounted := record
	unmounted.ParentIdentity = otherDevice
	if err := RemoveLocalTempNamespace(context.Background(), unmounted); !errors.Is(err, ErrTempNamespaceUnreachable) {
		t.Fatalf("missing path on another filesystem: got %v", err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLocalTempNamespace(context.Background(), unmounted); !errors.Is(err, ErrTempNamespaceUnreachable) {
		t.Fatalf("mount point on another filesystem: got %v", err)
	}
}

// Destination trees deeper than PATH_MAX are written component by component;
// removal and recovery reach their namespaces the same way.
func TestLocalTempNamespaceBeyondPathMax(t *testing.T) {
	base := t.TempDir()
	var parts []string
	for len(filepath.Join(append([]string{base}, parts...)...)) < 4200 {
		parts = append(parts, strings.Repeat("d", 100))
	}
	key := strings.Join(parts, "/") + "/file"
	registry := newEventRegistry(&eventLog{})
	local, err := NewLocal(base)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	if result, err := local.Write(context.Background(), key, strings.NewReader("deep"), WriteOptions{Size: 4, Overwrite: true, DeferDurability: true}); err != nil || !result.Committed {
		t.Fatalf("deep write: %+v %v", result, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	var namespace LocalTempNamespace
	for _, record := range registry.namespaces {
		namespace = record
	}
	if len(namespace.ParentPath) <= 4096 {
		t.Fatalf("test path is not deep enough: %d", len(namespace.ParentPath))
	}
	// An empty unregistered temporary file left by a crash is swept, then
	// the namespace is removed.
	parent, err := openAbsoluteDir(filepath.Join(namespace.ParentPath, namespace.Name))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.WriteFile(trackedTempName(namespace.Owner, strings.Repeat("ef", 16)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	parent.Close()
	if n, err := RemoveUnregisteredNamespaceTemps(context.Background(), namespace, nil); err != nil || n != 1 {
		t.Fatalf("deep recovery sweep: %d %v", n, err)
	}
	if err := RemoveLocalTempNamespace(context.Background(), namespace); err != nil {
		t.Fatalf("deep namespace removal: %v", err)
	}
	dir, err := openAbsoluteDir(namespace.ParentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if _, err := dir.Lstat(namespace.Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deep namespace remains: %v", err)
	}
}
