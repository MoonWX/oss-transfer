//go:build !windows

package transferstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A long destination path that passes through an in-root symlink leading
// outside the first path piece is accepted by the write path; removal and
// recovery must resolve it the same way.
func TestLongPathThroughSymlinkReachesNamespace(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var chain []string
	for len(filepath.Join(append([]string{base}, chain...)...)) < 1200 {
		chain = append(chain, strings.Repeat("c", 100))
	}
	// The chain is longer than PATH_MAX on some platforms; build it through a root.
	baseRoot, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRoot.Close()
	if err := baseRoot.MkdirAll(strings.Join(chain, "/"), 0o755); err != nil {
		t.Fatal(err)
	}
	// link -> ../../../real: climbs above the first path piece.
	if err := baseRoot.Symlink(strings.Repeat("../", len(chain))+"real", strings.Join(chain, "/")+"/link"); err != nil {
		t.Fatal(err)
	}
	key := strings.Join(chain, "/") + "/link/x/file"
	registry := newEventRegistry(&eventLog{})
	local, err := NewLocal(base)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	if result, err := local.Write(context.Background(), key, strings.NewReader("via link"), WriteOptions{Size: 8, Overwrite: true, DeferDurability: true}); err != nil || !result.Committed {
		t.Fatalf("write through symlink: %+v %v", result, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(base, "real", "x", "file")); err != nil || string(body) != "via link" {
		t.Fatalf("write did not follow the link: %q %v", body, err)
	}
	var namespace LocalTempNamespace
	for _, record := range registry.namespaces {
		namespace = record
	}
	if len(namespace.ParentPath) <= longPathPiece || !strings.Contains(namespace.ParentPath, "link") {
		t.Fatalf("test does not exercise a long path through the link: %q", namespace.ParentPath)
	}
	if n, err := RemoveUnregisteredNamespaceTemps(context.Background(), namespace, nil); err != nil || n != 0 {
		t.Fatalf("recovery sweep through the link: %d %v", n, err)
	}
	if err := RemoveLocalTempNamespace(context.Background(), namespace); err != nil {
		t.Fatalf("namespace removal through the link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "real", "x", namespace.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("namespace remains: %v", err)
	}
}
