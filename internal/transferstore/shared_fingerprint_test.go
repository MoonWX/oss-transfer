package transferstore

import (
	"bytes"
	"context"
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
	sample, err := SampleSharedFingerprints(ctx, a, 0)
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

func TestSharedFingerprintsDetectDifferentFiles(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	writeTree(t, a, sampleTree())
	sample, err := SampleSharedFingerprints(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	target := sample[0]
	other := func(t *testing.T) string {
		b := t.TempDir()
		writeTree(t, b, sampleTree())
		return b
	}
	t.Run("content", func(t *testing.T) {
		b := other(t)
		path := filepath.Join(b, filepath.FromSlash(target.Path))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) == 0 {
			content = []byte("x") // An empty file can only change by size.
		} else {
			content[len(content)-1] ^= 0xff
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := VerifySharedFingerprints(ctx, b, sample); !errors.Is(err, ErrSharedFingerprintChanged) {
			t.Fatal("changed file was not reported as changed", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		b := other(t)
		if err := os.Remove(filepath.Join(b, filepath.FromSlash(target.Path))); err != nil {
			t.Fatal(err)
		}
		err := VerifySharedFingerprints(ctx, b, sample)
		if !errors.Is(err, ErrSharedFingerprintMismatch) || errors.Is(err, ErrSharedFingerprintChanged) {
			t.Fatal("missing file must be a mismatch, not an edit", err)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		if err := VerifySharedFingerprints(ctx, t.TempDir(), sample); !errors.Is(err, ErrSharedFingerprintMismatch) {
			t.Fatal("an empty directory matched a populated sample", err)
		}
	})
}

func TestSharedFingerprintsHashFirstAndLastEdges(t *testing.T) {
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	large := bytes.Repeat([]byte("z"), 4*sharedFingerprintEdge)
	writeTree(t, a, map[string][]byte{"large.bin": large})
	sample, err := SampleSharedFingerprints(ctx, a, 0)
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
		if err := VerifySharedFingerprints(ctx, b, sample); !errors.Is(err, ErrSharedFingerprintChanged) {
			t.Fatalf("change at offset %d was not detected: %v", offset, err)
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
	sample, err := SampleSharedFingerprints(ctx, empty, 0)
	if err != nil || len(sample) != 0 {
		t.Fatal("empty root produced a sample", sample, err)
	}
	if err := VerifySharedFingerprints(ctx, alsoEmpty, sample); err != nil {
		t.Fatal("two empty roots must pass", err)
	}
	if err := VerifySharedFingerprints(ctx, populated, sample); !errors.Is(err, ErrSharedFingerprintMismatch) {
		t.Fatal("a populated root matched an empty one", err)
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
		sample, err := SampleSharedFingerprints(ctx, root, 0)
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
	sample, err := SampleSharedFingerprints(ctx, root, 0)
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
	if err := VerifySharedFingerprints(ctx, root, sample); !errors.Is(err, ErrSharedFingerprintChanged) {
		t.Fatal("a fresh read did not see the edit", err)
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
	if err := memo.Verify(ctx, root, sample); !errors.Is(err, ErrSharedFingerprintChanged) {
		t.Fatal("a changed file was answered from the memo", err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := memo.Verify(ctx, root, sample); !errors.Is(err, ErrSharedFingerprintMismatch) || errors.Is(err, ErrSharedFingerprintChanged) {
		t.Fatal("a missing file was answered from the memo", err)
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
	for _, name := range []string{"", ".", "/etc/passwd", "../outside", "dir/../../outside", "dir/./file", "dir//file", "dir/", `dir\file`, "C:/file", ".oss-transfer-shared-probe-x", "dir/.oss-transfer-1.tmp", "nul\x00byte", strings.Repeat("a", SharedFingerprintMaxPath+1)} {
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

func TestReadOnlyStorageClassification(t *testing.T) {
	wrap := func(err error) error { return &fs.PathError{Op: "open", Path: "/shared/x", Err: err} }
	for _, err := range []error{wrap(syscall.EACCES), wrap(syscall.EPERM), wrap(syscall.EROFS), fmt.Errorf("probe: %w", wrap(fs.ErrPermission))} {
		if !IsReadOnlyStorage(err) {
			t.Fatalf("%v is a read-only or permission error", err)
		}
	}
	for _, err := range []error{nil, wrap(syscall.ENOSPC), wrap(syscall.EIO), wrap(fs.ErrNotExist), errors.New("permission denied")} {
		if IsReadOnlyStorage(err) {
			t.Fatalf("%v is not a read-only or permission error", err)
		}
	}
	if runtime.GOOS == "windows" {
		if !IsReadOnlyStorage(wrap(syscall.Errno(19))) || !IsReadOnlyStorage(wrap(syscall.Errno(5))) {
			t.Fatal("Windows write protection and access denied are read-only errors")
		}
	}
}
