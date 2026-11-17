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

// Content fingerprints check a shared upload source that no Agent can write
// to: one Agent samples existing files, every other Agent must read the same
// size and the same first and last 64 KiB at the same relative paths. They are
// weaker than the write probe: separate copies holding identical files pass,
// and the middle of a large file is not read. They are only a fallback.
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
	// A slow mount (a FUSE file system over object storage, a distant NFS
	// server) may take seconds per directory. The walk stops after this long,
	// leaving time to read the sampled files within the Master's budget.
	sharedFingerprintWalkTime = 5 * time.Second
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
	// ErrSharedFingerprintUnsampled means the directory holds entries but no
	// file could be sampled, so it is neither compared nor known to be empty.
	ErrSharedFingerprintUnsampled = errors.New("no file in the shared directory could be sampled for comparison")
	// ErrSharedFingerprintIncomplete means a directory expected to hold no
	// file could not be listed completely, so its emptiness is not known.
	ErrSharedFingerprintIncomplete = errors.New("the shared directory could not be listed completely")
)

// What a verifying Agent found at a sampled path that does not match.
const (
	// SharedFingerprintChanged: a regular file with another size or another
	// first or last 64 KiB; the difference carries what this Agent read.
	SharedFingerprintChanged = "changed"
	// SharedFingerprintMissing: nothing exists at the path.
	SharedFingerprintMissing = "missing"
	// SharedFingerprintUnreadable: something exists but could not be read as
	// a regular file (permissions, I/O error, a directory or symlink, or a
	// name this Agent's operating system cannot open).
	SharedFingerprintUnreadable = "unreadable"
	// SharedFingerprintUnexpected: the sample held no file, but this Agent
	// found the file named by the difference.
	SharedFingerprintUnexpected = "unexpected"
)

// SharedFingerprint identifies one sampled file: its slash-separated path
// relative to the shared root, its size, and SHA-256 of its first and last
// 64 KiB (the whole file when it is at most 128 KiB).
type SharedFingerprint struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SharedFingerprintDifference is one sampled path that a verifying Agent sees
// differently. For a changed file it carries the size and digest this Agent
// read, so a Master can tell whether all Agents saw the same edit.
type SharedFingerprintDifference struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Size    int64  `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Message string `json:"message,omitempty"`
}

// SharedFingerprintMismatchError lists every sampled path this Agent sees
// differently. It matches ErrSharedFingerprintMismatch.
type SharedFingerprintMismatchError struct {
	Differences []SharedFingerprintDifference
}

func (e *SharedFingerprintMismatchError) Error() string {
	if len(e.Differences) == 0 {
		return ErrSharedFingerprintMismatch.Error()
	}
	first := e.Differences[0]
	var text string
	switch first.Status {
	case SharedFingerprintChanged:
		text = fmt.Sprintf("%q has another size or another first or last 64 KiB", first.Path)
	case SharedFingerprintMissing:
		text = fmt.Sprintf("%q is missing", first.Path)
	case SharedFingerprintUnexpected:
		text = fmt.Sprintf("the sample has no file, but %q exists", first.Path)
	default:
		text = fmt.Sprintf("%q cannot be read: %s", first.Path, first.Message)
	}
	if len(e.Differences) > 1 {
		text += fmt.Sprintf(" (and %d more)", len(e.Differences)-1)
	}
	return ErrSharedFingerprintMismatch.Error() + ": " + text
}

func (e *SharedFingerprintMismatchError) Is(target error) bool {
	return target == ErrSharedFingerprintMismatch
}

// portableFingerprintComponent reports whether a Windows Agent can open a path
// component under the same name: no characters Windows reserves, no trailing
// dot or space, and no device name. It follows platformKey on Windows.
func portableFingerprintComponent(component string) bool {
	if strings.ContainsAny(component, `:<>"|?*`) || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return false
	}
	for _, r := range component {
		if r < 32 {
			return false
		}
	}
	base := component
	if i := strings.IndexAny(base, ".:"); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		switch strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT") {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return false
		}
	}
	return true
}

// validFingerprintPath reports whether a relative path can be sent to other
// Agents and name the same file there: valid UTF-8 (JSON carries no other
// bytes), slash-separated inside the root, no backslash (a separator on
// Windows, and a name local discovery refuses elsewhere) and no tool-owned
// component. A portable path also avoids names Windows cannot open.
func validFingerprintPath(name string, portable bool) bool {
	if name == "" || len(name) > SharedFingerprintMaxPath || !utf8.ValidString(name) || !fs.ValidPath(name) || name == "." {
		return false
	}
	if strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, toolEntryPrefix) || portable && !portableFingerprintComponent(part) {
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
		if !validFingerprintPath(fingerprint.Path, false) || seen[fingerprint.Path] {
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
		return SharedFingerprint{}, nil, fmt.Errorf("%q is not a regular file", name)
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

// fingerprintWalk is what one bounded walk of a shared root saw.
type fingerprintWalk struct {
	// candidates are sampleable regular files drawn uniformly at random.
	candidates []string
	// files counts regular files, including those whose names cannot be
	// sampled; first names one of them.
	files int
	first string
	// unsampleable counts files and directories skipped for their names; the
	// contents of such a directory are not known.
	unsampleable int
	// unreadable counts subdirectories that could not be listed.
	unreadable int
	// limited means a budget stopped the walk before every entry was seen.
	limited bool
}

// complete reports whether the walk saw every entry of every directory.
func (w fingerprintWalk) complete() bool { return !w.limited && w.unreadable == 0 }

// describe says why the walk cannot vouch for every entry, for error messages.
func (w fingerprintWalk) describe() string {
	var reasons []string
	if w.unsampleable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d files or directories have names that cannot be compared on every node (not valid UTF-8, containing a backslash, or not openable on Windows)", w.unsampleable))
	}
	if w.unreadable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d directories could not be listed", w.unreadable))
	}
	if w.limited {
		reasons = append(reasons, fmt.Sprintf("the listing stopped at its limit of %d directories, %d entries, %d entries per directory or %d seconds", sharedFingerprintWalkDirectories, sharedFingerprintWalkEntries, sharedFingerprintDirectoryBatch, int(sharedFingerprintWalkTime/time.Second)))
	}
	if len(reasons) == 0 {
		return "no regular file was found"
	}
	return strings.Join(reasons, "; ")
}

// walkFingerprintCandidates lists a root in random order within fixed
// budgets. With sample set it draws up to limit sampleable regular files
// uniformly at random (reservoir sampling), counts entries whose names cannot
// be sampled and does not descend into such directories; without it, it only
// looks for regular files of any name. Directories are read in batches, so a
// single huge directory is seen from its first entries instead of being
// listed in full. Symlinks, special files and the tool's own entries are
// skipped, as discovery skips them.
func walkFingerprintCandidates(ctx context.Context, root *os.Root, limit int, sample, portable bool) (fingerprintWalk, error) {
	var walk fingerprintWalk
	seen := 0
	pending := []string{"."}
	entries, directories := 0, 0
	stop := time.Now().Add(sharedFingerprintWalkTime)
	for len(pending) > 0 {
		if entries >= sharedFingerprintWalkEntries || directories >= sharedFingerprintWalkDirectories || directories > 0 && time.Now().After(stop) {
			walk.limited = true
			break
		}
		if err := ctx.Err(); err != nil {
			return walk, err
		}
		index := randomBelow(len(pending))
		directory := pending[index]
		pending[index] = pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directories++
		file, err := root.Open(directory)
		if err != nil {
			if directory == "." {
				return walk, err
			}
			walk.unreadable++
			continue
		}
		list, err := file.ReadDir(sharedFingerprintDirectoryBatch)
		file.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			if directory == "." {
				return walk, err
			}
			walk.unreadable++
			continue
		}
		if len(list) == sharedFingerprintDirectoryBatch {
			walk.limited = true // Entries after the first batch are not seen.
		}
		for _, entry := range list {
			entries++
			if strings.HasPrefix(entry.Name(), toolEntryPrefix) {
				continue
			}
			name := path.Join(directory, entry.Name())
			usable := !sample || validFingerprintPath(name, portable)
			switch {
			case entry.IsDir():
				if usable {
					pending = append(pending, name)
				} else {
					walk.unsampleable++
				}
			case entry.Type().IsRegular():
				walk.files++
				if walk.first == "" {
					walk.first = name
				}
				if !usable {
					walk.unsampleable++
					continue
				}
				if limit < 1 {
					continue
				}
				seen++
				if len(walk.candidates) < limit {
					walk.candidates = append(walk.candidates, name)
				} else if index := randomBelow(seen); index < limit {
					walk.candidates[index] = name
				}
			}
		}
	}
	shuffleNames(walk.candidates)
	return walk, nil
}

// SampleSharedFingerprints picks up to limit existing regular files under
// directory and fingerprints them. Portable restricts the sample to names a
// Windows Agent can open. An empty result means that a complete listing found
// no regular file at all; when files or directories exist but none could be
// sampled, it returns ErrSharedFingerprintUnsampled instead, so a directory is
// never vouched for as empty after a partial look.
func SampleSharedFingerprints(ctx context.Context, directory string, limit int, portable bool) ([]SharedFingerprint, error) {
	if limit < 1 || limit > SharedFingerprintMaxFiles {
		limit = SharedFingerprintMaxFiles
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	walk, err := walkFingerprintCandidates(ctx, root, limit*sharedFingerprintCandidates, true, portable)
	if err != nil {
		return nil, err
	}
	// Files that changed recently go last: another Agent reads them later
	// and must not see a different version.
	var settled, recent []string
	var readErr error
	now := time.Now()
	for _, name := range walk.candidates {
		info, err := root.Lstat(name)
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%q is not a regular file", name)
		}
		if err != nil {
			readErr = cmpOr(readErr, err)
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
			readErr = cmpOr(readErr, err)
			continue // Removed, replaced or unreadable since the walk; try another.
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	switch {
	case len(fingerprints) > 0:
		return fingerprints, nil
	case len(walk.candidates) > 0:
		return nil, fmt.Errorf("%w: %d files were found but none could be read: %v", ErrSharedFingerprintUnsampled, len(walk.candidates), readErr)
	case walk.files > 0 || walk.unsampleable > 0 || !walk.complete():
		return nil, fmt.Errorf("%w: %s", ErrSharedFingerprintUnsampled, walk.describe())
	}
	return fingerprints, nil
}

func cmpOr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// VerifySharedFingerprints recomputes every expected fingerprint through this
// Agent's root. Every sampled path is checked, and the differences are
// returned together as a *SharedFingerprintMismatchError. An empty expectation
// requires a complete listing that finds no regular file of any name.
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
		walk, err := walkFingerprintCandidates(ctx, root, 0, false, false)
		if err != nil {
			return err
		}
		if walk.files > 0 {
			return &SharedFingerprintMismatchError{Differences: []SharedFingerprintDifference{{Path: walk.first, Status: SharedFingerprintUnexpected}}}
		}
		if !walk.complete() {
			return fmt.Errorf("%w: %s", ErrSharedFingerprintIncomplete, walk.describe())
		}
		return nil
	}
	var differences []SharedFingerprintDifference
	for _, want := range expected {
		if !platformFingerprintPath(want.Path) {
			differences = append(differences, SharedFingerprintDifference{Path: want.Path, Status: SharedFingerprintUnreadable, Message: "this Agent's operating system cannot open this name"})
			continue
		}
		got, err := memo.fingerprint(ctx, root, directory, want.Path)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			differences = append(differences, SharedFingerprintDifference{Path: want.Path, Status: SharedFingerprintMissing})
		case err != nil:
			differences = append(differences, SharedFingerprintDifference{Path: want.Path, Status: SharedFingerprintUnreadable, Message: err.Error()})
		case got.Size != want.Size || got.SHA256 != want.SHA256:
			differences = append(differences, SharedFingerprintDifference{Path: want.Path, Status: SharedFingerprintChanged, Size: got.Size, SHA256: got.SHA256})
		}
	}
	if len(differences) > 0 {
		return &SharedFingerprintMismatchError{Differences: differences}
	}
	return nil
}

// platformFingerprintPath reports whether this Agent's operating system can
// open a sampled path under its exact name.
func platformFingerprintPath(name string) bool {
	if !windowsNames {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if !portableFingerprintComponent(part) {
			return false
		}
	}
	return true
}

// SharedFingerprintMemo lets an Agent re-verify the same files cheaply: the
// Master re-checks a shared source no node can write on every task pass. A digest is
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
