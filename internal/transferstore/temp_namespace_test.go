package transferstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func makeTestNamespace(t *testing.T) (string, *os.Root, LocalTempNamespace) {
	t.Helper()
	path := t.TempDir()
	parent, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })
	record, err := CreateLocalTempNamespace(context.Background(), parent, path, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	return path, parent, record
}
func TestTempNamespaceRemovesOnlyEmptyUnregisteredOwnedFiles(t *testing.T) {
	path, _, record := makeTestNamespace(t)
	base := filepath.Join(path, record.Name)
	empty := trackedTempName(record.Owner, strings.Repeat("11", 16))
	registered := trackedTempName(record.Owner, strings.Repeat("22", 16))
	foreign := trackedTempName(strings.Repeat("cd", 32), strings.Repeat("33", 16))
	for _, name := range []string{empty, registered, foreign, "ordinary-user-file"} {
		if err := os.WriteFile(filepath.Join(base, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, empty), nil, 0600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveUnregisteredNamespaceTemps(context.Background(), record, map[string]bool{filepath.Join(base, registered): true})
	if err != nil || removed != 1 {
		t.Fatalf("cleanup=%d %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(base, empty)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("own empty file not removed: %v", err)
	}
	for _, name := range []string{filepath.Join(base, registered), filepath.Join(base, foreign), filepath.Join(base, "ordinary-user-file"), filepath.Join(path, empty)} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("cleanup touched unowned path %s: %v", name, err)
		}
	}
	nonempty := trackedTempName(record.Owner, strings.Repeat("44", 16))
	if err := os.WriteFile(filepath.Join(base, nonempty), []byte("untracked content"), 0600); err != nil {
		t.Fatal(err)
	}
	removed, err = RemoveUnregisteredNamespaceTemps(context.Background(), record, map[string]bool{filepath.Join(base, registered): true})
	if err == nil || removed != 0 {
		t.Fatalf("unregistered payload must be retained: %d %v", removed, err)
	}
	if data, err := os.ReadFile(filepath.Join(base, nonempty)); err != nil || string(data) != "untracked content" {
		t.Fatalf("untracked content changed: %q %v", data, err)
	}
}

func TestTempNamespaceRejectsReplacementAndMarkerChanges(t *testing.T) {
	path, parent, record := makeTestNamespace(t)
	namespace := filepath.Join(path, record.Name)
	name := trackedTempName(record.Owner, strings.Repeat("11", 16))
	if err := os.Rename(namespace, namespace+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, namespaceMarker), []byte(namespaceContents(record.Owner, record.ID)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, name), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenLocalTempNamespace(parent, record); err == nil {
		opened.Close()
		t.Fatal("replacement directory was adopted")
	}
	if n, err := RemoveUnregisteredNamespaceTemps(context.Background(), record, nil); err == nil || n != 0 {
		t.Fatalf("replacement was cleaned: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(namespace, name)); err != nil {
		t.Fatal("foreign replacement file was removed")
	}
	// Restore the actual directory, then tamper with the ownership marker.
	if err := os.Rename(namespace, namespace+"-replacement"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(namespace+"-original", namespace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, namespaceMarker), []byte("not an internal marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenLocalTempNamespace(parent, record); err == nil {
		opened.Close()
		t.Fatal("changed marker was accepted")
	}
}

func TestLocalScanExcludesOnlyCompleteNamespaceMarkers(t *testing.T) {
	root, _, record := makeTestNamespace(t)
	real := filepath.Join(root, record.Name)
	if err := os.WriteFile(filepath.Join(real, trackedTempName(record.Owner, strings.Repeat("ab", 16))), []byte("private incomplete payload"), 0600); err != nil {
		t.Fatal(err)
	}
	// Even a perfectly shaped name remains a user directory without the exact marker.
	fakeName := namespaceName(record.Owner, strings.Repeat("ff", 16))
	fake := filepath.Join(root, fakeName)
	if err := os.Mkdir(fake, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "user"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "normal"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	var keys []string
	if err := local.Scan(context.Background(), ListOptions{}, func(object Object) error { keys = append(keys, object.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{fakeName + "/user", "normal"}
	slices.Sort(want)
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Fatalf("scan=%v want=%v", keys, want)
	}
	var listed []string
	opts := ListOptions{Limit: 1}
	for {
		page, err := local.List(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Objects {
			listed = append(listed, o.Key)
		}
		if page.NextCursor == "" {
			break
		}
		opts.Cursor = page.NextCursor
	}
	slices.Sort(listed)
	if !slices.Equal(listed, want) {
		t.Fatalf("list=%v want=%v", listed, want)
	}
}

func TestTempPublishPinnedDirectoryHandles(t *testing.T) {
	base := t.TempDir()
	sourcePath, targetPath := filepath.Join(base, "source"), filepath.Join(base, "target")
	for _, path := range []string{sourcePath, targetPath} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	baseRoot, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRoot.Close()
	// Match production: child roots are opened relative to the destination
	// root. On Windows, unlike absolute OpenRoot, these allow directory rename.
	source, err := baseRoot.OpenRoot("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := baseRoot.OpenRoot("target")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := os.WriteFile(filepath.Join(sourcePath, "temporary"), []byte("correct"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := source.Stat("temporary")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := storageIdentity(source, "temporary", info)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, targetPath} {
		if err := os.Rename(path, path+"-pinned"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "temporary"), []byte("substitute"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishTempAcrossRoots(source, target, "temporary", "complete", true, identity); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(targetPath+"-pinned", "complete")); err != nil || string(data) != "correct" {
		t.Fatalf("published wrong source/destination: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(targetPath, "complete")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("replaced path was used")
	}
	if data, err := os.ReadFile(filepath.Join(sourcePath, "temporary")); err != nil || string(data) != "substitute" {
		t.Fatal("replacement source was moved")
	}
}
