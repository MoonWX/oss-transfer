//go:build linux

package legacyimport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPublishDirectoryNeverReplacesExistingDestination(t *testing.T) {
	original := renameNoReplace
	t.Cleanup(func() { renameNoReplace = original })
	for name, rename := range map[string]func(string, string) error{
		"renameat2": original,
		// Early CentOS / RHEL 7 kernels, and file systems without the flag.
		"without renameat2":        func(string, string) error { return unix.ENOSYS },
		"without RENAME_NOREPLACE": func(string, string) error { return unix.EINVAL },
	} {
		t.Run(name, func(t *testing.T) {
			renameNoReplace = rename
			root := t.TempDir()
			from := filepath.Join(root, "staged")
			if err := os.Mkdir(from, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(from, "manifest.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, existing := range []string{"empty", "nonempty"} {
				to := filepath.Join(root, existing)
				if err := os.Mkdir(to, 0700); err != nil {
					t.Fatal(err)
				}
				if existing == "nonempty" {
					if err := os.WriteFile(filepath.Join(to, "keep"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := publishDirectory(from, to); !errors.Is(err, fs.ErrExist) {
					t.Fatalf("%s destination: got %v, want an existing-file error", existing, err)
				}
				if _, err := os.Stat(filepath.Join(from, "manifest.json")); err != nil {
					t.Fatalf("staged directory changed after refusing %s destination: %v", existing, err)
				}
			}
			if raw, err := os.ReadFile(filepath.Join(root, "nonempty", "keep")); err != nil || string(raw) != "keep" {
				t.Fatalf("existing destination changed: %q, %v", raw, err)
			}
			to := filepath.Join(root, "published")
			if err := publishDirectory(from, to); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(to, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(from); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("staged directory still exists: %v", err)
			}
		})
	}
}
