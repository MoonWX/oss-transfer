package transferstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A tree whose files all sit under ISO timestamps, as backup jobs write them.
// Discovery transfers such names on Unix Agents, so they must be compared
// rather than mistaken for an empty source.
func TestSharedFingerprintsCompareColonNamesUnlessPortable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file systems cannot hold ':' in names")
	}
	ctx := context.Background()
	root, empty := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string][]byte{
		"2026-11-15T02:00:00Z/db.sql.gz": []byte("monday"),
		"2026-11-16T02:00:00Z/db.sql.gz": []byte("tuesday"),
	})
	sample, err := SampleSharedFingerprints(ctx, root, 0, false)
	if err != nil || len(sample) != 2 {
		t.Fatal("colon names were not sampled", sample, err)
	}
	if err := ValidateSharedFingerprints(sample); err != nil {
		t.Fatal("a colon path must be a valid request between Unix Agents", err)
	}
	if found := differences(t, VerifySharedFingerprints(ctx, empty, sample)); len(found) != 2 {
		t.Fatal("an empty directory matched a tree of colon names", found)
	}
	// With a Windows Agent taking part the names cannot be compared, and the
	// tree must not pass as empty.
	if sample, err := SampleSharedFingerprints(ctx, root, 0, true); !errors.Is(err, ErrSharedFingerprintUnsampled) || sample != nil {
		t.Fatal("a tree of names Windows cannot open was reported as empty", sample, err)
	}
	// A verifier given an empty sample finds files of any name.
	if found := differences(t, VerifySharedFingerprints(ctx, root, nil)); !strings.Contains(found[firstPath(found)].Path, "db.sql.gz") || found[firstPath(found)].Status != SharedFingerprintUnexpected {
		t.Fatal("files under colon names were not found", found)
	}
}

func firstPath(found map[string]SharedFingerprintDifference) string {
	for path := range found {
		return path
	}
	return ""
}

func TestSharedFingerprintsNeverSendNamesOtherAgentsCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file systems cannot hold these names")
	}
	ctx := context.Background()
	root := t.TempDir()
	// macOS refuses names that are not UTF-8; other Unix systems accept them.
	names := []string{`back\slash.bin`, "bad-\xff-utf8.bin"}
	created := 0
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err == nil {
			created++
		}
	}
	if created == 0 {
		t.Skip("this file system accepts none of these names")
	}
	if sample, err := SampleSharedFingerprints(ctx, root, 0, false); !errors.Is(err, ErrSharedFingerprintUnsampled) || !strings.Contains(err.Error(), "names that cannot be compared") {
		t.Fatal("unsendable names were sampled or treated as an empty directory", sample, err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.bin"), []byte("plain"), 0600); err != nil {
		t.Fatal(err)
	}
	sample, err := SampleSharedFingerprints(ctx, root, 0, false)
	if err != nil || len(sample) != 1 || sample[0].Path != "plain.bin" {
		t.Fatal("the sendable file was not the only one sampled", sample, err)
	}
}

func TestSharedFingerprintsPortableNames(t *testing.T) {
	for _, name := range []string{"a.b", "conx", "com10", "nested/file.txt", "lpt", "日本語.txt"} {
		if !validFingerprintPath(name, true) {
			t.Fatalf("%q is portable", name)
		}
	}
	for _, name := range []string{"a:b", "dir/CON", "nul.txt", "NUL .txt", "com1", "LPT¹.log", "trailing.", "trailing ", "what?", "pipe|name", "tab\tname"} {
		if validFingerprintPath(name, true) {
			t.Fatalf("%q cannot be opened on Windows", name)
		}
		if strings.ContainsAny(name, ":") && !validFingerprintPath(name, false) {
			t.Fatalf("%q is a valid name between Unix Agents", name)
		}
	}
}

// An unreadable subdirectory or a listing cut short by its budget hides
// entries; such a directory must never pass as empty.
func TestSharedFingerprintsOnlyTreatACompleteListingAsEmpty(t *testing.T) {
	ctx := context.Background()
	t.Run("listing limit", func(t *testing.T) {
		root := t.TempDir()
		for i := range sharedFingerprintWalkDirectories + 8 {
			if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%03d", i/20), fmt.Sprintf("e%02d", i%20)), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if sample, err := SampleSharedFingerprints(ctx, root, 0, false); !errors.Is(err, ErrSharedFingerprintUnsampled) || !strings.Contains(err.Error(), "limit") {
			t.Fatal("a listing stopped by its limit was reported as empty", sample, err)
		}
		if err := VerifySharedFingerprints(ctx, root, nil); !errors.Is(err, ErrSharedFingerprintIncomplete) || errors.Is(err, ErrSharedFingerprintMismatch) {
			t.Fatal("a verifier vouched for an incompletely listed directory", err)
		}
	})
	t.Run("permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs POSIX permissions enforced for this account")
		}
		root := t.TempDir()
		writeTree(t, root, map[string][]byte{"locked/data.bin": []byte("data")})
		locked := filepath.Join(root, "locked")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
		if sample, err := SampleSharedFingerprints(ctx, root, 0, false); !errors.Is(err, ErrSharedFingerprintUnsampled) || !strings.Contains(err.Error(), "could not be listed") {
			t.Fatal("an unreadable directory was reported as empty", sample, err)
		}
		if err := VerifySharedFingerprints(ctx, root, nil); !errors.Is(err, ErrSharedFingerprintIncomplete) {
			t.Fatal("a verifier vouched for a directory it could not list", err)
		}
		if err := os.Chmod(locked, 0700); err != nil {
			t.Fatal(err)
		}
		// Files that can be listed but not read are found, not ignored.
		file := filepath.Join(locked, "data.bin")
		if err := os.Chmod(file, 0); err != nil {
			t.Fatal(err)
		}
		if sample, err := SampleSharedFingerprints(ctx, root, 0, false); !errors.Is(err, ErrSharedFingerprintUnsampled) || !strings.Contains(err.Error(), "none could be read") {
			t.Fatal("files that cannot be read were reported as an empty directory", sample, err)
		}
		if found := differences(t, VerifySharedFingerprints(ctx, root, nil)); found["locked/data.bin"].Status != SharedFingerprintUnexpected {
			t.Fatal("a verifier did not find a file it cannot read", found)
		}
	})
}
