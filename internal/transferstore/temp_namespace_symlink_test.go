//go:build !windows

package transferstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func longChain(base string, length int) []string {
	var chain []string
	for len(filepath.Join(append([]string{base}, chain...)...)) < length {
		chain = append(chain, strings.Repeat("c", 100))
	}
	return chain
}

// writeNamespace writes key through a tracked Local and returns the staging
// namespace it created.
func writeNamespace(t *testing.T, base, key string) LocalTempNamespace {
	t.Helper()
	registry := newEventRegistry(&eventLog{})
	local, err := NewLocal(base)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	if result, err := local.Write(context.Background(), key, strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true, DeferDurability: true}); err != nil || !result.Committed {
		t.Fatalf("write %s: %+v %v", key, result, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	var namespace LocalTempNamespace
	for _, record := range registry.namespaces {
		namespace = record
	}
	return namespace
}

// A long recorded path through a symlink in its component-by-component part
// is never resolved by guesswork: link targets such as "s/../y" (where s is
// itself a link) need physical "..". The namespace is reported unreachable,
// so its record is kept and retried, and is never mistaken for deleted.
func TestLongPathThroughSymlinkIsUnreachableNotGone(t *testing.T) {
	base := t.TempDir()
	// Longer than PATH_MAX everywhere (4096 on Linux), so the kernel cannot
	// open it and the component walk must refuse the link.
	chain := longChain(base, 4300)
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	deep := strings.Join(chain, "/")
	// s -> elsewhere/inner; link -> s/../y. Physically link is elsewhere/y;
	// a lexical join would wrongly pick <deep>/y, the decoy.
	for _, dir := range []string{deep + "/y", "elsewhere/inner", "elsewhere/y"} {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Symlink(strings.Repeat("../", len(chain))+"elsewhere/inner", deep+"/s"); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("s/../y", deep+"/link"); err != nil {
		t.Fatal(err)
	}
	namespace := writeNamespace(t, base, deep+"/link/file")
	if body, err := os.ReadFile(filepath.Join(base, "elsewhere", "y", "file")); err != nil || string(body) != "payload" {
		t.Fatalf("write did not follow the links physically: %q %v", body, err)
	}
	if len(namespace.ParentPath) <= 900 || !strings.Contains(namespace.ParentPath, "link") {
		t.Fatalf("test does not exercise a long path through the link: %d", len(namespace.ParentPath))
	}
	err = RemoveLocalTempNamespace(context.Background(), namespace)
	if errors.Is(err, ErrTempNamespaceGone) || !errors.Is(err, ErrTempNamespaceUnreachable) {
		t.Fatalf("long symlinked path: got %v, want unreachable", err)
	}
	if _, err := RemoveUnregisteredNamespaceTemps(context.Background(), namespace, nil); !errors.Is(err, errLongPathSymlink) {
		t.Fatalf("recovery sweep must not guess through the link: %v", err)
	}
	// The real namespace stays where the write put it.
	if _, err := os.Lstat(filepath.Join(base, "elsewhere", "y", namespace.Name)); err != nil {
		t.Fatalf("namespace was touched: %v", err)
	}
}

// A symlink in the first piece of a long path is resolved by the kernel. On
// Linux that works even when its physical expansion is longer than PATH_MAX.
func TestLongPathWithLongPhysicalPrefixIsReachable(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	physical := strings.Join(longChain(base, 1100), "/")
	if err := root.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(physical, "l"); err != nil {
		t.Fatal(err)
	}
	tail := strings.Join(longChain(filepath.Join(base, "l"), 1300), "/")
	if err := root.MkdirAll("l/"+tail, 0o755); err != nil {
		t.Fatal(err)
	}
	namespace := writeNamespace(t, base, "l/"+tail+"/file")
	if !strings.HasPrefix(namespace.ParentPath, filepath.Join(base, "l")+string(filepath.Separator)) {
		t.Fatalf("unexpected recorded path: %.120s", namespace.ParentPath)
	}
	if runtime.GOOS != "linux" {
		// The darwin kernel limits the expanded path too; removal cannot
		// reach it by path, but must fail safe rather than call it deleted.
		if err := RemoveLocalTempNamespace(context.Background(), namespace); err == nil || errors.Is(err, ErrTempNamespaceGone) {
			t.Fatalf("unreachable long physical path reported as %v", err)
		}
		return
	}
	if n, err := RemoveUnregisteredNamespaceTemps(context.Background(), namespace, nil); err != nil || n != 0 {
		t.Fatalf("recovery sweep: %d %v", n, err)
	}
	if err := RemoveLocalTempNamespace(context.Background(), namespace); err != nil {
		t.Fatalf("namespace removal: %v", err)
	}
	dir, err := root.OpenRoot("l/" + tail)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if _, err := dir.Lstat(namespace.Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("namespace remains: %v", err)
	}
}

// Paths over the walk threshold but within PATH_MAX are opened by the kernel,
// following an in-tree symlink exactly as the write path did, so their
// namespaces are still removed.
func TestMidLengthPathThroughSymlinkIsRemoved(t *testing.T) {
	// Resolve the temp dir itself (/var -> /private/var on darwin) so the
	// kernel's own symlink expansion stays within darwin's 1024-byte limit.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chain := longChain(base, 905)
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	deep := strings.Join(chain, "/")
	if err := root.MkdirAll(deep+"/real", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("real", deep+"/link"); err != nil {
		t.Fatal(err)
	}
	namespace := writeNamespace(t, base, deep+"/link/file")
	if n := len(namespace.ParentPath); n <= 900 || n >= 1010 {
		t.Fatalf("test needs a ParentPath between 900 and 1010 bytes, got %d", n)
	}
	if err := RemoveLocalTempNamespace(context.Background(), namespace); err != nil {
		t.Fatalf("mid-length symlinked namespace not removed: %v", err)
	}
	if _, err := root.Lstat(deep + "/real/" + namespace.Name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("namespace directory still present: %v", err)
	}
}
