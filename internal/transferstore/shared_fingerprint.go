package transferstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Content fingerprints prove a read-only shared source without writing to it:
// one Agent samples existing files, every other Agent must read the same bytes
// at the same relative paths. They are weaker than the write probe (a copy of
// the same tree passes), so they are only a fallback for read-only uploads.
const (
	SharedFingerprintMaxFiles = 16
	SharedFingerprintMaxPath  = 4096
	sharedFingerprintEdge     = 64 << 10
	// The walk stops after this many directory entries or directories, so a
	// tree with millions of files is sampled, never listed in full. It reads
	// directories only; files are inspected after the random selection.
	sharedFingerprintWalkEntries     = 10000
	sharedFingerprintWalkDirectories = 512
	sharedFingerprintDirectoryBatch  = 1024
	// The walk keeps this many candidates per requested file, so that files
	// that changed recently can be passed over without biasing the sample.
	sharedFingerprintCandidates = 4
	// A file modified this recently may still be written while the other
	// Agents read it; such files are used only when nothing older was drawn.
	sharedFingerprintSettled = 2 * time.Minute
	// Every name the tool creates in a user directory starts with this prefix:
	// shared probes, temporary files and staging namespaces.
	toolEntryPrefix = ".oss-transfer-"
	// sharedFingerprintMemoEntries bounds the digests an Agent remembers.
	sharedFingerprintMemoEntries = 4096
)

var (
	ErrSharedFingerprint         = errors.New("shared directory fingerprint request is invalid")
	ErrSharedFingerprintMismatch = errors.New("shared directory contents do not match")
	// ErrSharedFingerprintChanged is a mismatch where the sampled file still
	// exists but its size or content differ: the file may have been edited
	// rather than the directory replaced.
	ErrSharedFingerprintChanged = fmt.Errorf("%w: a sampled file changed", ErrSharedFingerprintMismatch)
)

// SharedFingerprint identifies one sampled file: its slash-separated path
// relative to the shared root, its size, and SHA-256 of its first and last
// 64 KiB (the whole file when it is at most 128 KiB).
type SharedFingerprint struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func validFingerprintPath(name string) bool {
	if name == "" || len(name) > SharedFingerprintMaxPath || !utf8.ValidString(name) || !fs.ValidPath(name) || name == "." {
		return false
	}
	// Backslashes and colons change meaning on Windows Agents; such a path
	// cannot name the same file on every platform.
	if strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, toolEntryPrefix) {
			return false
		}
	}
	return true
}

// ValidateSharedFingerprints bounds a verification request before any file is
// opened: at most SharedFingerprintMaxFiles relative paths, no "..", no
// absolute or tool-owned paths, and well-formed sizes and digests.
func ValidateSharedFingerprints(fingerprints []SharedFingerprint) error {
	if len(fingerprints) > SharedFingerprintMaxFiles {
		return fmt.Errorf("%w: at most %d files", ErrSharedFingerprint, SharedFingerprintMaxFiles)
	}
	seen := make(map[string]bool, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if !validFingerprintPath(fingerprint.Path) || seen[fingerprint.Path] {
			return fmt.Errorf("%w: invalid path %q", ErrSharedFingerprint, fingerprint.Path)
		}
		seen[fingerprint.Path] = true
		if fingerprint.Size < 0 || len(fingerprint.SHA256) != sha256.Size*2 || strings.ToLower(fingerprint.SHA256) != fingerprint.SHA256 {
			return fmt.Errorf("%w: invalid fingerprint for %q", ErrSharedFingerprint, fingerprint.Path)
		}
		if _, err := hex.DecodeString(fingerprint.SHA256); err != nil {
			return fmt.Errorf("%w: invalid fingerprint for %q", ErrSharedFingerprint, fingerprint.Path)
		}
	}
	return nil
}

// fingerprintFile hashes one regular file through root. It refuses symlinks
// and files replaced or changed while being read, so a fingerprint always
// describes one stable file inside the root. It also returns that file's
// final metadata for SharedFingerprintMemo.
func fingerprintFile(ctx context.Context, root *os.Root, name string) (SharedFingerprint, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return SharedFingerprint{}, nil, err
	}
	if !before.Mode().IsRegular() {
		return SharedFingerprint{}, nil, fmt.Errorf("%w: %q is not a regular file", ErrSharedFingerprintMismatch, name)
	}
	file, err := root.Open(name)
	if err != nil {
		return SharedFingerprint{}, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return SharedFingerprint{}, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return SharedFingerprint{}, nil, fmt.Errorf("%w: %q changed while it was read", ErrSourceChanged, name)
	}
	size := opened.Size()
	hash := sha256.New()
	reader := contextReader{ctx, file}
	if size <= 2*sharedFingerprintEdge {
		if _, err := io.Copy(hash, io.LimitReader(reader, size)); err != nil {
			return SharedFingerprint{}, nil, err
		}
	} else {
		if _, err := io.CopyN(hash, reader, sharedFingerprintEdge); err != nil {
			return SharedFingerprint{}, nil, err
		}
		if _, err := io.Copy(hash, io.NewSectionReader(file, size-sharedFingerprintEdge, sharedFingerprintEdge)); err != nil {
			return SharedFingerprint{}, nil, err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return SharedFingerprint{}, nil, err
	}
	if after.Size() != size || !after.ModTime().Equal(opened.ModTime()) {
		return SharedFingerprint{}, nil, fmt.Errorf("%w: %q changed while it was read", ErrSourceChanged, name)
	}
	return SharedFingerprint{Path: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, after, nil
}

func randomBelow(n int) int {
	var buffer [8]byte
	_, _ = rand.Read(buffer[:])
	return int(binary.LittleEndian.Uint64(buffer[:]) % uint64(n))
}

func shuffleNames(names []string) {
	for i := len(names) - 1; i > 0; i-- {
		j := randomBelow(i + 1)
		names[i], names[j] = names[j], names[i]
	}
}

// collectFingerprintCandidates draws up to limit regular files uniformly at
// random (reservoir sampling) from a walk in random order within fixed
// budgets. Directories are read in batches, so a single huge directory is
// sampled from its first entries instead of being listed in full. Symlinks
// and special files are skipped, as discovery skips them.
func collectFingerprintCandidates(ctx context.Context, root *os.Root, limit int) ([]string, error) {
	var candidates []string
	seen := 0
	pending := []string{"."}
	entries, directories := 0, 0
	for len(pending) > 0 && entries < sharedFingerprintWalkEntries && directories < sharedFingerprintWalkDirectories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index := randomBelow(len(pending))
		directory := pending[index]
		pending[index] = pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directories++
		file, err := root.Open(directory)
		if err != nil {
			if directory == "." {
				return nil, err
			}
			continue // An unreadable subdirectory is not evidence either way.
		}
		list, err := file.ReadDir(sharedFingerprintDirectoryBatch)
		file.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			if directory == "." {
				return nil, err
			}
			continue
		}
		for _, entry := range list {
			entries++
			if strings.HasPrefix(entry.Name(), toolEntryPrefix) {
				continue
			}
			name := path.Join(directory, entry.Name())
			if !validFingerprintPath(name) {
				continue
			}
			switch {
			case entry.IsDir():
				pending = append(pending, name)
			case entry.Type().IsRegular():
				seen++
				if len(candidates) < limit {
					candidates = append(candidates, name)
				} else if index := randomBelow(seen); index < limit {
					candidates[index] = name
				}
			}
		}
	}
	shuffleNames(candidates)
	return candidates, nil
}

// SampleSharedFingerprints picks up to limit existing regular files under
// directory and fingerprints them. An empty result means the bounded walk
// found no file, which a migration source with nothing to transfer allows.
func SampleSharedFingerprints(ctx context.Context, directory string, limit int) ([]SharedFingerprint, error) {
	if limit < 1 || limit > SharedFingerprintMaxFiles {
		limit = SharedFingerprintMaxFiles
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	candidates, err := collectFingerprintCandidates(ctx, root, limit*sharedFingerprintCandidates)
	if err != nil {
		return nil, err
	}
	// Files that changed recently go last: another Agent reads them later
	// and must not see a different version.
	var settled, recent []string
	now := time.Now()
	for _, name := range candidates {
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) >= sharedFingerprintSettled {
			settled = append(settled, name)
		} else {
			recent = append(recent, name)
		}
	}
	fingerprints := []SharedFingerprint{}
	for _, name := range append(settled, recent...) {
		if len(fingerprints) == limit {
			break
		}
		fingerprint, _, err := fingerprintFile(ctx, root, name)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue // Removed, replaced or unreadable since the walk; try another.
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	return fingerprints, nil
}

// VerifySharedFingerprints recomputes every expected fingerprint through this
// Agent's root. An empty expectation requires this Agent to find no file
// either, so an empty directory is not mistaken for a populated one.
func VerifySharedFingerprints(ctx context.Context, directory string, expected []SharedFingerprint) error {
	return verifySharedFingerprints(ctx, directory, expected, nil)
}

func verifySharedFingerprints(ctx context.Context, directory string, expected []SharedFingerprint, memo *SharedFingerprintMemo) error {
	if err := ValidateSharedFingerprints(expected); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if len(expected) == 0 {
		names, err := collectFingerprintCandidates(ctx, root, 1)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return fmt.Errorf("%w: the sampling node found no file, this node found %q", ErrSharedFingerprintMismatch, names[0])
		}
		return nil
	}
	for _, want := range expected {
		got, err := memo.fingerprint(ctx, root, directory, want.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %q is missing", ErrSharedFingerprintMismatch, want.Path)
		}
		if err != nil {
			return err
		}
		if got.Size != want.Size {
			return fmt.Errorf("%w: %q has %d bytes, expected %d", ErrSharedFingerprintChanged, want.Path, got.Size, want.Size)
		}
		if got.SHA256 != want.SHA256 {
			return fmt.Errorf("%w: %q has different content", ErrSharedFingerprintChanged, want.Path)
		}
	}
	return nil
}

// SharedFingerprintMemo lets an Agent re-verify the same files cheaply: the
// Master re-checks a read-only shared source on every task pass. A digest is
// reused only while the path still names the same file (device and inode)
// with the same size and modification time, which a different or remounted
// directory does not. The zero value is ready to use.
type SharedFingerprintMemo struct {
	mu      sync.Mutex
	entries map[string]memoizedFingerprint
}

type memoizedFingerprint struct {
	info        fs.FileInfo
	fingerprint SharedFingerprint
}

// Verify is VerifySharedFingerprints with digests remembered across calls.
func (m *SharedFingerprintMemo) Verify(ctx context.Context, directory string, expected []SharedFingerprint) error {
	return verifySharedFingerprints(ctx, directory, expected, m)
}

func (m *SharedFingerprintMemo) fingerprint(ctx context.Context, root *os.Root, directory, name string) (SharedFingerprint, error) {
	if m == nil {
		fingerprint, _, err := fingerprintFile(ctx, root, name)
		return fingerprint, err
	}
	key := filepath.Clean(directory) + "\x00" + name
	current, err := root.Lstat(name)
	if err != nil {
		return SharedFingerprint{}, err
	}
	m.mu.Lock()
	known, found := m.entries[key]
	m.mu.Unlock()
	if found && current.Mode().IsRegular() && os.SameFile(known.info, current) && current.Size() == known.info.Size() && current.ModTime().Equal(known.info.ModTime()) {
		return known.fingerprint, nil
	}
	fingerprint, info, err := fingerprintFile(ctx, root, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.entries, key)
		return SharedFingerprint{}, err
	}
	if m.entries == nil || len(m.entries) >= sharedFingerprintMemoEntries {
		m.entries = map[string]memoizedFingerprint{}
	}
	m.entries[key] = memoizedFingerprint{info: info, fingerprint: fingerprint}
	return fingerprint, nil
}
