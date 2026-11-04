package transferstore

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Staging namespaces come and go while other nodes scan the same tree. An
// entry deleted after its directory was listed is skipped, not fatal.
func TestLocalDiscoverySkipsEntriesDeletedDuringTheWalk(t *testing.T) {
	setup := func(t *testing.T) string {
		root := t.TempDir()
		for _, name := range []string{"a/1", "a/2", "a/3", "b/1", "c"} {
			if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	// Deleting while a/1 is emitted: a/2 was already listed in a, and b in
	// the root, so the walk meets both after they are gone.
	vanish := func(root string) {
		_ = os.Remove(filepath.Join(root, "a", "2"))
		_ = os.RemoveAll(filepath.Join(root, "b"))
	}
	want := []string{"a/1", "a/3", "c"}
	run := func(t *testing.T, scan func(*Local, func(string)) error) {
		root := setup(t)
		local, err := NewLocal(root)
		if err != nil {
			t.Fatal(err)
		}
		defer local.Close()
		var keys []string
		err = scan(local, func(key string) {
			keys = append(keys, key)
			if key == "a/1" {
				vanish(root)
			}
		})
		if err != nil {
			t.Fatalf("walk failed on a deleted entry: %v", err)
		}
		// A file deleted after its directory entry was read may still be
		// reported from that entry; copying it later fails as not found.
		keys = slices.DeleteFunc(keys, func(key string) bool { return key == "a/2" })
		if !slices.Equal(keys, want) {
			t.Fatalf("got %v want %v", keys, want)
		}
	}
	t.Run("Scan", func(t *testing.T) {
		run(t, func(local *Local, emit func(string)) error {
			return local.Scan(context.Background(), ListOptions{}, func(object Object) error { emit(object.Key); return nil })
		})
	})
	t.Run("ScanPages", func(t *testing.T) {
		run(t, func(local *Local, emit func(string)) error {
			// One entry per page, so deletion happens between page reads.
			return local.ScanPages(context.Background(), ListOptions{Limit: 1}, func(page Page) error {
				for _, object := range page.Objects {
					emit(object.Key)
				}
				return nil
			})
		})
	})
}
