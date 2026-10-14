//go:build windows

package transferstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsTempPublicationUsesPinnedDirectories(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"source", "target"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	baseRoot, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRoot.Close()
	// The runtime opens staging directories with Root.OpenRoot, whose NT
	// handle permits rename. Absolute os.OpenRoot has different sharing flags.
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
	f, err := source.OpenFile("temporary", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("verified payload"); err != nil {
		t.Fatal(err)
	}
	identity, err := handleIdentity(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Replace both original pathnames after opening the roots. Publication must
	// continue to use the opened directories, never the replacement pathnames.
	for _, name := range []string{"source", "target"} {
		if err := os.Rename(filepath.Join(base, name), filepath.Join(base, name+"-moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "source", "temporary"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishTempAcrossRoots(source, target, "temporary", "目标.bin", false, identity); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(base, "target-moved", "目标.bin"))
	if err != nil || string(content) != "verified payload" {
		t.Fatalf("target: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(base, "target", "目标.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement directory used: %v", err)
	}
	if err := publishTempAcrossRoots(source, target, "temporary", "目标.bin", false, identity); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("SKIP replaced existing target: %v", err)
	}
	if err := publishTempAcrossRoots(source, target, "temporary", "rejected", true, identity+"-stale"); !errors.Is(err, ErrTempIdentity) {
		t.Fatalf("stale identity published: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "target-moved", "replace.bin"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishTempAcrossRoots(source, target, "temporary", "replace.bin", true, identity); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(base, "target-moved", "replace.bin"))
	if err != nil || string(content) != "verified payload" {
		t.Fatalf("overwrite: %q, %v", content, err)
	}
	if _, err := source.Stat("temporary"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rename did not remove source: %v", err)
	}
}
