package transferstore

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
)

const sharedProbePrefix = ".oss-transfer-shared-probe-"

var ErrSharedProbe = errors.New("shared storage probe is invalid or does not match")
var sharedProbeMutation sync.Mutex

// SharedProbe is a one-use proof that selected Agent roots expose the same
// directory. The coordinator must authorize each root before invoking these
// helpers, verify every selected Agent, then clean up before discovery starts.
// It is not a storage snapshot or a permanent guarantee that a mount remains.
type SharedProbe struct {
	Key   string `json:"key"`
	Nonce string `json:"nonce"`
}

func probeContents(probe SharedProbe) ([]byte, error) {
	if !strings.HasPrefix(probe.Key, sharedProbePrefix) || !strings.HasSuffix(probe.Key, ".probe") {
		return nil, ErrSharedProbe
	}
	name := strings.TrimSuffix(strings.TrimPrefix(probe.Key, sharedProbePrefix), ".probe")
	for _, value := range []string{name, probe.Nonce} {
		if len(value) != 64 || strings.ToLower(value) != value {
			return nil, ErrSharedProbe
		}
		if decoded, err := hex.DecodeString(value); err != nil || len(decoded) != 32 {
			return nil, ErrSharedProbe
		}
	}
	return []byte("oss-transfer-shared-probe-v1\n" + probe.Nonce + "\n"), nil
}

func ValidateSharedProbe(probe SharedProbe) error {
	_, err := probeContents(probe)
	return err
}

// SharedProbeRoot pins the directory used by a preflight probe. Its persistent
// identity prevents a temporarily unmounted path from being mistaken for the
// original directory when deciding whether cleanup has finished.
type SharedProbeRoot struct {
	root     *os.Root
	identity string
}

func OpenSharedProbeRoot(directory string) (*SharedProbeRoot, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	identity, err := storageIdentity(root, ".", info)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &SharedProbeRoot{root: root, identity: identity}, nil
}

func (r *SharedProbeRoot) Identity() string { return r.identity }
func (r *SharedProbeRoot) Close() error     { return r.root.Close() }

// The owner must serialize mutations of a probe identity. Different probes do
// not share a lock, so a stalled mount does not block unrelated preflights.
func (r *SharedProbeRoot) Ensure(ctx context.Context, probe SharedProbe) error {
	return ensureSharedProbe(ctx, r.root, probe)
}

func (r *SharedProbeRoot) Cleanup(ctx context.Context, probe SharedProbe) error {
	return cleanupSharedProbe(ctx, r.root, probe)
}

// isSharedProbeFile hides only authentic-format tool markers from a concurrent
// scan, including when a task scans an ancestor of another task's shared root.
// Files that merely resemble the prefix remain normal user objects.
func isSharedProbeFile(ctx context.Context, root *os.Root, key string, info fs.FileInfo) (bool, error) {
	name := path.Base(key)
	if !strings.HasPrefix(name, sharedProbePrefix) {
		return false, nil
	}
	probe := SharedProbe{Key: name, Nonce: strings.Repeat("0", 64)}
	template, err := probeContents(probe)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(template)) {
		return false, nil
	}
	file, err := root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil // The concurrent proof cleanup already removed this entry.
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return false, ErrSourceChanged
	}
	contents, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, int64(len(template)+1)))
	if err != nil {
		return false, err
	}
	const header = "oss-transfer-shared-probe-v1\n"
	if len(contents) != len(template) || !strings.HasPrefix(string(contents), header) || contents[len(contents)-1] != '\n' {
		return false, nil
	}
	probe.Nonce = string(contents[len(header) : len(contents)-1])
	expected, err := probeContents(probe)
	return err == nil && subtle.ConstantTimeCompare(contents, expected) == 1, nil
}

// NewSharedProbe generates the marker identity before any Agent RPC. Persist it
// before EnsureSharedProbe so a lost reply never leaves an unknown marker name.
func NewSharedProbe() (SharedProbe, error) {
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return SharedProbe{}, err
	}
	return SharedProbe{Key: sharedProbePrefix + hex.EncodeToString(random[:32]) + ".probe", Nonce: hex.EncodeToString(random[32:])}, nil
}

// CreateSharedProbe is a convenience for local callers. RPC coordinators should
// use NewSharedProbe + EnsureSharedProbe so the requested identity is replayable.
func CreateSharedProbe(ctx context.Context, directory string) (SharedProbe, error) {
	probe, err := NewSharedProbe()
	if err == nil {
		err = EnsureSharedProbe(ctx, directory, probe)
	}
	if err != nil {
		return SharedProbe{}, err
	}
	return probe, nil
}

// EnsureSharedProbe creates the given marker on the designated writer Agent, or
// verifies the exact existing marker on RPC replay. It does not create roots or
// replace any existing content. Mutating retries within this process serialize.
func EnsureSharedProbe(ctx context.Context, directory string, probe SharedProbe) error {
	sharedProbeMutation.Lock()
	defer sharedProbeMutation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureSharedProbe(ctx, root, probe)
}

func ensureSharedProbe(ctx context.Context, root *os.Root, probe SharedProbe) error {
	contents, err := probeContents(probe)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Readable across Agent users; the random nonce is a probe, not a credential.
	file, err := root.OpenFile(probe.Key, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, fs.ErrExist) {
		_, err = readSharedProbe(ctx, root, probe)
		return err
	}
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			_ = root.Remove(probe.Key)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := syncPublished(root, probe.Key); err != nil {
		return err
	}
	complete = true
	return nil
}

func readSharedProbe(ctx context.Context, root *os.Root, probe SharedProbe) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expected, err := probeContents(probe)
	if err != nil {
		return nil, err
	}
	before, err := root.Lstat(probe.Key)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() != int64(len(expected)) {
		return nil, ErrSharedProbe
	}
	file, err := root.Open(probe.Key)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrSharedProbe
	}
	actual, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, int64(len(expected)+1)))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || subtle.ConstantTimeCompare(actual, expected) != 1 {
		return nil, ErrSharedProbe
	}
	return after, nil
}

// VerifySharedProbe reads the marker through this Agent's path. Missing files,
// mismatched content, symlinks, and different roots fail the proof.
func VerifySharedProbe(ctx context.Context, directory string, probe SharedProbe) error {
	if _, err := probeContents(probe); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	_, err = readSharedProbe(ctx, root, probe)
	return err
}

// CleanupSharedProbe removes only a marker matching the exact nonce and strict
// generated filename format. An absent marker is already clean. This API is not
// a general file-delete operation and does not follow symlinks.
func CleanupSharedProbe(ctx context.Context, directory string, probe SharedProbe) error {
	if _, err := probeContents(probe); err != nil {
		return err
	}
	sharedProbeMutation.Lock()
	defer sharedProbeMutation.Unlock()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return cleanupSharedProbe(ctx, root, probe)
}

func cleanupSharedProbe(ctx context.Context, root *os.Root, probe SharedProbe) error {
	verified, err := readSharedProbe(ctx, root, probe)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := root.Lstat(probe.Key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(verified, current) || current.Size() != verified.Size() || !current.ModTime().Equal(verified.ModTime()) {
		return ErrSharedProbe
	}
	if err := root.Remove(probe.Key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove shared storage probe: %w", err)
	}
	return nil
}
