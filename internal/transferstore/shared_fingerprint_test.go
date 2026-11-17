package transferstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func sampleTree() map[string][]byte {
	files := map[string][]byte{
		"large.bin":         bytes.Repeat([]byte("0123456789abcdef"), 40<<10), // 640 KiB
		"exact-edges.bin":   bytes.Repeat([]byte{7}, 2*sharedFingerprintEdge),
		"empty.txt":         {},
		"nested/deep/a.txt": []byte("alpha"),
	}
	for i := range 30 {
		files[fmt.Sprintf("dir%d/file%d.txt", i%5, i)] = []byte(fmt.Sprintf("content %d", i))
	}
	return files
}

func TestSharedFingerprintsMatchIdenticalTrees(t *testing.T) {
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, sampleTree())
	writeTree(t, b, sampleTree())
	sample, err := SampleSharedFingerprints(ctx, a, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) != SharedFingerprintMaxFiles {
		t.Fatalf("sampled %d files, want %d", len(sample), SharedFingerprintMaxFiles)
	}
	if err := ValidateSharedFingerprints(sample); err != nil {
		t.Fatal("sample does not pass its own request limits", err)
	}
	for _, root := range []string{a, b} {
		if err := VerifySharedFingerprints(ctx, root, sample); err != nil {
			t.Fatal(err)
		}
	}
}

// differences returns the per-path statuses of a mismatch, failing the test
// for any other error.
func differences(t *testing.T, err error) map[string]SharedFingerprintDifference {
	t.Helper()
	var mismatch *SharedFingerprintMismatchError
	if !errors.As(err, &mismatch) || !errors.Is(err, ErrSharedFingerprintMismatch) {
		t.Fatalf("want a fingerprint mismatch, got %v", err)
	}
	found := map[string]SharedFingerprintDifference{}
	for _, difference := range mismatch.Differences {
		found[difference.Path] = difference
	}
	return found
}

func TestSharedFingerprintsDetectDifferentFiles(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	writeTree(t, a, sampleTree())
	sample, err := SampleSharedFingerprints(ctx, a, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	other := func(t *testing.T) string {
		b := t.TempDir()
		writeTree(t, b, sampleTree())
		return b
	}
	t.Run("every difference is reported with its kind", func(t *testing.T) {
		b := other(t)
		changed, missing, replaced := sample[0], sample[1], sample[2]
		content := []byte("a different, longer content")
		if err := os.WriteFile(filepath.Join(b, filepath.FromSlash(changed.Path)), content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(b, filepath.FromSlash(missing.Path))); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(b, filepath.FromSlash(replaced.Path))); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(b, filepath.FromSlash(replaced.Path)), 0700); err != nil {
			t.Fatal(err)
		}
		found := differences(t, VerifySharedFingerprints(ctx, b, sample))
		if len(found) != 3 {
			t.Fatal("want exactly the three differences", found)
		}
		got := found[changed.Path]
		digest := sha256.Sum256(content)
		if got.Status != SharedFingerprintChanged || got.Size != int64(len(content)) || got.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatal("a changed file must carry what this node read", got)
		}
		if found[missing.Path].Status != SharedFingerprintMissing {
			t.Fatal("a missing file must be reported as missing", found[missing.Path])
		}
		if found[replaced.Path].Status != SharedFingerprintUnreadable || found[replaced.Path].Message == "" {
			t.Fatal("a directory in place of a file must be unreadable, with a reason", found[replaced.Path])
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		found := differences(t, VerifySharedFingerprints(ctx, t.TempDir(), sample))
		for _, fingerprint := range sample {
			if found[fingerprint.Path].Status != SharedFingerprintMissing {
				t.Fatal("an empty directory must miss every sampled file", found)
			}
		}
	})
}

func TestSharedFingerprintsHashFirstAndLastEdges(t *testing.T) {
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	large := bytes.Repeat([]byte("z"), 4*sharedFingerprintEdge)
	writeTree(t, a, map[string][]byte{"large.bin": large})
	sample, err := SampleSharedFingerprints(ctx, a, 0, false)
	if err != nil || len(sample) != 1 {
		t.Fatal(sample, err)
	}
	// The middle is not read, which keeps sampling cheap on huge files.
	middle := append([]byte(nil), large...)
	middle[2*sharedFingerprintEdge] = 'm'
	writeTree(t, b, map[string][]byte{"large.bin": middle})
	if err := VerifySharedFingerprints(ctx, b, sample); err != nil {
		t.Fatal("middle bytes are outside the fingerprint", err)
	}
	for _, offset := range []int{0, sharedFingerprintEdge - 1, len(large) - sharedFingerprintEdge, len(large) - 1} {
		edge := append([]byte(nil), large...)
		edge[offset] = 'e'
		writeTree(t, b, map[string][]byte{"large.bin": edge})
		if found := differences(t, VerifySharedFingerprints(ctx, b, sample)); found["large.bin"].Status != SharedFingerprintChanged {
			t.Fatalf("change at offset %d was not detected: %v", offset, found)
		}
	}
}

func TestSharedFingerprintsEmptyRoot(t *testing.T) {
	ctx := context.Background()
	empty, alsoEmpty, populated := t.TempDir(), t.TempDir(), t.TempDir()
	// Only directories and the tool's own entries: nothing to transfer.
	if err := os.MkdirAll(filepath.Join(empty, "a", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, empty, map[string][]byte{".oss-transfer-0123.tmp": []byte("temporary")})
	writeTree(t, populated, map[string][]byte{"data.txt": []byte("data")})
	sample, err := SampleSharedFingerprints(ctx, empty, 0, false)
	if err != nil || len(sample) != 0 {
		t.Fatal("empty root produced a sample", sample, err)
	}
	if err := VerifySharedFingerprints(ctx, alsoEmpty, sample); err != nil {
		t.Fatal("two empty roots must pass", err)
	}
	if found := differences(t, VerifySharedFingerprints(ctx, populated, sample)); found["data.txt"].Status != SharedFingerprintUnexpected {
		t.Fatal("a populated root matched an empty one", found)
	}
}

func TestSharedFingerprintsSkipToolFilesAndSymlinks(t *testing.T) {
	ctx := context.Background()
	root, outside := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string][]byte{
		"real.txt":                         []byte("real"),
		".oss-transfer-shared-probe-x":     []byte("probe"),
		".oss-transfer-staging-x/file.tmp": []byte("staged"),
	})
	writeTree(t, outside, map[string][]byte{"secret.txt": []byte("secret")})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		sample, err := SampleSharedFingerprints(ctx, root, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(sample) != 1 || sample[0].Path != "real.txt" {
			t.Fatal("sample included a tool file or a symlink", sample)
		}
	}
	// A verifier never follows a symlink, even one a valid path names.
	fingerprint, _, err := fingerprintFile(ctx, mustOpenRoot(t, outside), "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link.txt", "linked-dir/secret.txt"} {
		fingerprint.Path = name
		if err := VerifySharedFingerprints(ctx, root, []SharedFingerprint{fingerprint}); err == nil {
			t.Fatalf("verification followed %s outside the root", name)
		}
	}
}

func TestSharedFingerprintMemoReusesOnlyTheSameUnchangedFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	name := filepath.Join(root, "data.bin")
	writeTree(t, root, map[string][]byte{"data.bin": []byte("original")})
	sample, err := SampleSharedFingerprints(ctx, root, 0, false)
	if err != nil || len(sample) != 1 {
		t.Fatal(sample, err)
	}
	var memo SharedFingerprintMemo
	if err := memo.Verify(ctx, root, sample); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	// An in-place edit that keeps size and time is still the same file in
	// the same directory: the memo answers without reading it again.
	file, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("ORIGINAL"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := memo.Verify(ctx, root, sample); err != nil {
		t.Fatal("unchanged file metadata did not reuse the digest", err)
	}
	if found := differences(t, VerifySharedFingerprints(ctx, root, sample)); found["data.bin"].Status != SharedFingerprintChanged {
		t.Fatal("a fresh read did not see the edit", found)
	}
	// A replacement is another file, even with the same size and time.
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, name); err != nil {
		t.Fatal(err)
	}
	if err := memo.Verify(ctx, root, sample); err != nil {
		t.Fatal("the replacement has the sampled content and must match", err)
	}
	if err := os.WriteFile(name, []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	// Set the time explicitly: a coarse file system clock could repeat it.
	if err := os.Chtimes(name, info.ModTime().Add(time.Hour), info.ModTime().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if found := differences(t, memo.Verify(ctx, root, sample)); found["data.bin"].Status != SharedFingerprintChanged {
		t.Fatal("a changed file was answered from the memo", found)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if found := differences(t, memo.Verify(ctx, root, sample)); found["data.bin"].Status != SharedFingerprintMissing {
		t.Fatal("a missing file was answered from the memo", found)
	}
}

func mustOpenRoot(t *testing.T, directory string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestSharedFingerprintRequestsAreBounded(t *testing.T) {
	valid := SharedFingerprint{Path: "dir/file.txt", Size: 1, SHA256: strings.Repeat("a", 64)}
	if err := ValidateSharedFingerprints([]SharedFingerprint{valid}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "/etc/passwd", "../outside", "dir/../../outside", "dir/./file", "dir//file", "dir/", `dir\file`, ".oss-transfer-shared-probe-x", "dir/.oss-transfer-1.tmp", "nul\x00byte", "bad-\xff-utf8", strings.Repeat("a", SharedFingerprintMaxPath+1)} {
		bad := valid
		bad.Path = name
		if err := ValidateSharedFingerprints([]SharedFingerprint{bad}); !errors.Is(err, ErrSharedFingerprint) {
			t.Fatalf("path %q was accepted", name)
		}
	}
	for _, bad := range []SharedFingerprint{
		{Path: "a", Size: -1, SHA256: valid.SHA256},
		{Path: "a", Size: 1, SHA256: strings.Repeat("A", 64)},
		{Path: "a", Size: 1, SHA256: strings.Repeat("g", 64)},
		{Path: "a", Size: 1, SHA256: "abc"},
	} {
		if err := ValidateSharedFingerprints([]SharedFingerprint{bad}); !errors.Is(err, ErrSharedFingerprint) {
			t.Fatalf("fingerprint %+v was accepted", bad)
		}
	}
	if err := ValidateSharedFingerprints([]SharedFingerprint{valid, valid}); !errors.Is(err, ErrSharedFingerprint) {
		t.Fatal("duplicate paths were accepted", err)
	}
	many := make([]SharedFingerprint, SharedFingerprintMaxFiles+1)
	for i := range many {
		many[i] = valid
		many[i].Path = fmt.Sprintf("f%d", i)
	}
	if err := ValidateSharedFingerprints(many); !errors.Is(err, ErrSharedFingerprint) {
		t.Fatal("oversized request was accepted", err)
	}
	if err := VerifySharedFingerprints(context.Background(), t.TempDir(), many); !errors.Is(err, ErrSharedFingerprint) {
		t.Fatal("verification opened files for an invalid request", err)
	}
}

func TestUnwritableStorageClassification(t *testing.T) {
	wrap := func(err error) error { return &fs.PathError{Op: "open", Path: "/shared/x", Err: err} }
	for _, err := range []error{wrap(syscall.EACCES), wrap(syscall.EPERM), wrap(syscall.EROFS), wrap(syscall.ENOSPC), wrap(syscall.EDQUOT), fmt.Errorf("probe: %w", wrap(fs.ErrPermission))} {
		if !IsUnwritableStorage(err) {
			t.Fatalf("%v means the root cannot be written", err)
		}
	}
	for _, err := range []error{nil, wrap(syscall.EIO), wrap(fs.ErrNotExist), errors.New("permission denied")} {
		if IsUnwritableStorage(err) {
			t.Fatalf("%v does not mean the root cannot be written", err)
		}
	}
	if runtime.GOOS == "windows" {
		for _, code := range []syscall.Errno{5, 19, 39, 112, 1295} {
			if !IsUnwritableStorage(wrap(code)) {
				t.Fatalf("Windows error %d means the root cannot be written", code)
			}
		}
	}
}
