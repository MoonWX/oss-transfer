package transferstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrTempIdentity = errors.New("tracked temporary file identity does not match; file retained")

// LocalTempRecord identifies one file created by a single Agent database owner.
// Identity excludes size/mtime, which necessarily change while writing payload.
type LocalTempRecord struct {
	Owner          string `json:"owner"`
	ID             string `json:"id"`
	ParentPath     string `json:"parentPath"`
	ParentIdentity string `json:"parentIdentity"`
	Name           string `json:"name"`
	FileIdentity   string `json:"fileIdentity"`
}

type LocalTempRegistry interface {
	OwnerID() string
	Register(context.Context, LocalTempRecord) error
	Forget(context.Context, string) error
}

// SetTempRegistry must be called before the Local is used by any goroutine.
// The Agent supplies a durable registry owned by its exclusively opened database.
func (s *Local) SetTempRegistry(registry LocalTempRegistry) { s.tempRegistry = registry }

func trackedTempName(owner, id string) string { return ".oss-transfer-" + owner + "-" + id + ".tmp" }

func validHex(value string, bytes int) bool {
	if len(value) != bytes*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}

func ValidateLocalTempRecord(record LocalTempRecord) error {
	if !validHex(record.Owner, 32) || !validHex(record.ID, 16) || record.Name != trackedTempName(record.Owner, record.ID) ||
		!filepath.IsAbs(record.ParentPath) || strings.ContainsRune(record.ParentPath, '\x00') || record.ParentIdentity == "" || record.FileIdentity == "" {
		return ErrTempIdentity
	}
	return nil
}

// RemoveTrackedLocalTemp accesses only the exact recorded parent and filename.
// Directory replacement, mount changes, symlinks, and file replacement are
// rejected. Call only during startup while the database owner has no active I/O.
func RemoveTrackedLocalTemp(ctx context.Context, record LocalTempRecord) (bool, error) {
	return removeLocalTemp(ctx, record, false)
}

func removeLocalTemp(ctx context.Context, record LocalTempRecord, emptyOnly bool) (bool, error) {
	if err := ValidateLocalTempRecord(record); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(record.ParentPath)
	if err != nil {
		return false, err // A missing mount may return; retain its durable record.
	}
	defer root.Close()
	parent, err := root.Stat(".")
	if err != nil {
		return false, err
	}
	identity, err := storageIdentity(root, ".", parent)
	if err != nil {
		return false, err
	}
	if identity != record.ParentIdentity {
		return false, ErrTempIdentity
	}
	removed, err := removeLocalTempEntry(ctx, root, record.Name, record.FileIdentity, emptyOnly)
	if err == nil && removed {
		err = syncTempNamespace(root)
	}
	return removed, err
}

// Remove only the original entry under a pinned directory. Callers retain the
// durable record when a same-name replacement or incomplete cleanup is found.
func removeLocalTempEntry(ctx context.Context, root *os.Root, name, expectedIdentity string, emptyOnly bool) (bool, error) {
	if expectedIdentity == "" {
		return false, ErrTempIdentity
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || (emptyOnly && info.Size() != 0) {
		return false, ErrTempIdentity
	}
	identity, err := storageIdentity(root, name, info)
	if err != nil {
		return false, err
	}
	if identity != expectedIdentity {
		return false, ErrTempIdentity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) || (emptyOnly && current.Size() != 0) {
		return false, ErrTempIdentity
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
