package transferstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const namespacePrefix = ".oss-transfer-staging-"
const namespaceMarker = ".oss-transfer-namespace"

// A namespace is registered durably before it may contain temporary files.
// This closes the file-create -> file-identity registration crash window.
type LocalTempNamespace struct {
	Owner             string `json:"owner"`
	ID                string `json:"id"`
	ParentPath        string `json:"parentPath"`
	ParentIdentity    string `json:"parentIdentity"`
	Name              string `json:"name"`
	DirectoryIdentity string `json:"directoryIdentity"`
}

type LocalTempNamespaceRegistry interface {
	PrepareNamespace(context.Context, *os.Root, string) (LocalTempNamespace, error)
}

func namespaceName(owner, id string) string { return namespacePrefix + owner + "-" + id }
func namespaceContents(owner, id string) string {
	return "oss-transfer temporary namespace v1\n" + owner + "\n" + id + "\n"
}

func ValidateLocalTempNamespace(record LocalTempNamespace) error {
	if !validHex(record.Owner, 32) || !validHex(record.ID, 16) || record.Name != namespaceName(record.Owner, record.ID) ||
		!filepath.IsAbs(record.ParentPath) || strings.ContainsRune(record.ParentPath, '\x00') || record.ParentIdentity == "" || record.DirectoryIdentity == "" {
		return ErrTempIdentity
	}
	return nil
}

func CreateLocalTempNamespace(ctx context.Context, parent *os.Root, parentPath, owner string) (LocalTempNamespace, error) {
	var record LocalTempNamespace
	if !validHex(owner, 32) || !filepath.IsAbs(parentPath) {
		return record, ErrTempIdentity
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	info, err := parent.Stat(".")
	if err != nil {
		return record, err
	}
	identity, err := storageIdentity(parent, ".", info)
	if err != nil {
		return record, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return record, err
	}
	id := hex.EncodeToString(nonce[:])
	name := namespaceName(owner, id)
	if err := createTempNamespace(parent, name); err != nil {
		return record, err
	}
	// A crash here can leave an empty unregistered directory. Never adopt or
	// remove such a directory just because its name resembles ours.
	directory, err := parent.OpenRoot(name)
	if err != nil {
		return record, err
	}
	defer directory.Close()
	if err := secureTempNamespace(directory); err != nil {
		return record, err
	}
	info, err = directory.Stat(".")
	if err != nil {
		return record, err
	}
	directoryIdentity, err := storageIdentity(directory, ".", info)
	if err != nil {
		return record, err
	}
	marker, err := directory.OpenFile(namespaceMarker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return record, err
	}
	_, writeErr := io.WriteString(marker, namespaceContents(owner, id))
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return record, err
	}
	if err := syncPublished(directory, namespaceMarker); err != nil {
		return record, err
	}
	if err := syncTempNamespace(parent); err != nil {
		return record, err
	}
	record = LocalTempNamespace{Owner: owner, ID: id, ParentPath: parentPath, ParentIdentity: identity, Name: name, DirectoryIdentity: directoryIdentity}
	check, err := OpenLocalTempNamespace(parent, record)
	if err != nil {
		return record, err
	}
	check.Close()
	return record, nil
}

// OpenLocalTempNamespace pins the original parent and rejects replacements,
// symlinks, changed permissions and missing/modified ownership markers.
func OpenLocalTempNamespace(parent *os.Root, record LocalTempNamespace) (*os.Root, error) {
	if err := ValidateLocalTempNamespace(record); err != nil {
		return nil, err
	}
	info, err := parent.Stat(".")
	if err != nil {
		return nil, err
	}
	identity, err := storageIdentity(parent, ".", info)
	if err != nil {
		return nil, err
	}
	if identity != record.ParentIdentity {
		return nil, ErrTempIdentity
	}
	info, err = parent.Lstat(record.Name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrTempIdentity
	}
	directory, err := parent.OpenRoot(record.Name)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			directory.Close()
		}
	}()
	info, err = directory.Stat(".")
	if err != nil {
		return nil, err
	}
	identity, err = storageIdentity(directory, ".", info)
	if err != nil {
		return nil, err
	}
	if identity != record.DirectoryIdentity {
		return nil, ErrTempIdentity
	}
	if err := verifyTempNamespace(directory); err != nil {
		return nil, err
	}
	if !validNamespaceMarker(directory, record.Owner, record.ID) {
		return nil, ErrTempIdentity
	}
	valid = true
	return directory, nil
}

func validNamespaceMarker(directory *os.Root, owner, id string) bool {
	info, err := directory.Lstat(namespaceMarker)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	expected := namespaceContents(owner, id)
	if info.Size() != int64(len(expected)) {
		return false
	}
	file, err := directory.Open(namespaceMarker)
	if err != nil {
		return false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	return err == nil && string(data) == expected
}

func isLocalTempNamespace(root *os.Root, key string) bool {
	name := filepath.Base(filepath.FromSlash(key))
	if !strings.HasPrefix(name, namespacePrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(name, namespacePrefix), "-")
	if len(parts) != 2 || !validHex(parts[0], 32) || !validHex(parts[1], 16) {
		return false
	}
	directory, err := root.OpenRoot(key)
	if err != nil {
		return false
	}
	defer directory.Close()
	return validNamespaceMarker(directory, parts[0], parts[1])
}

// RemoveUnregisteredNamespaceTemps visits only one database-owned namespace.
// It removes only empty, regular, owner-matching files that have no durable file
// record. Registered/replaced files and nonempty or foreign files are retained.
func RemoveUnregisteredNamespaceTemps(ctx context.Context, record LocalTempNamespace, registered map[string]bool) (int, error) {
	parent, err := os.OpenRoot(record.ParentPath)
	if err != nil {
		return 0, err
	}
	defer parent.Close()
	directory, err := OpenLocalTempNamespace(parent, record)
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	stream, err := directory.Open(".")
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	removed := 0
	for {
		entries, readErr := stream.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			name := entry.Name()
			prefix := ".oss-transfer-" + record.Owner + "-"
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tmp")
			if !validHex(id, 16) || registered[filepath.Join(record.ParentPath, record.Name, name)] {
				continue
			}
			info, err := directory.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return removed, fmt.Errorf("unregistered nonempty or unsafe temporary file retained: %s", name)
			}
			identity, err := storageIdentity(directory, name, info)
			if err != nil {
				return removed, err
			}
			temp := LocalTempRecord{Owner: record.Owner, ID: id, ParentPath: filepath.Join(record.ParentPath, record.Name), ParentIdentity: record.DirectoryIdentity, Name: name, FileIdentity: identity}
			ok, err := removeLocalTemp(ctx, temp, true)
			if err != nil {
				return removed, err
			}
			if ok {
				removed++
			}
		}
		if errors.Is(readErr, io.EOF) {
			return removed, nil
		}
		if readErr != nil {
			return removed, readErr
		}
	}
}

// ErrTempNamespaceInUse means a staging namespace still holds entries other
// than its ownership marker, so it was not removed.
var ErrTempNamespaceInUse = errors.New("temporary namespace is not empty; retained")

// RemoveLocalTempNamespace deletes a database-owned staging namespace that
// holds nothing but its marker. It also finishes a removal interrupted after
// the marker was deleted, and succeeds when the namespace is already gone from
// its original parent. Callers must guarantee that no temporary file in it is
// in use or still registered.
func RemoveLocalTempNamespace(ctx context.Context, record LocalTempNamespace) error {
	if err := ValidateLocalTempNamespace(record); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := os.OpenRoot(record.ParentPath)
	if err != nil {
		return err // A missing mount may return; retain the durable record.
	}
	defer parent.Close()
	info, err := parent.Stat(".")
	if err != nil {
		return err
	}
	identity, err := storageIdentity(parent, ".", info)
	if err != nil {
		return err
	}
	if identity != record.ParentIdentity {
		return ErrTempIdentity
	}
	info, err = parent.Lstat(record.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return ErrTempIdentity
	}
	if err := clearTempNamespace(parent, record); err != nil {
		return err
	}
	if err := parent.Remove(record.Name); err != nil {
		return err
	}
	return syncTempNamespace(parent)
}

// clearTempNamespace removes the marker of a verified, otherwise empty
// namespace. Its handles are closed before the caller removes the directory.
func clearTempNamespace(parent *os.Root, record LocalTempNamespace) error {
	directory, err := parent.OpenRoot(record.Name)
	if err != nil {
		return err
	}
	defer directory.Close()
	info, err := directory.Stat(".")
	if err != nil {
		return err
	}
	identity, err := storageIdentity(directory, ".", info)
	if err != nil {
		return err
	}
	if identity != record.DirectoryIdentity {
		return ErrTempIdentity
	}
	if err := verifyTempNamespace(directory); err != nil {
		return err
	}
	stream, err := directory.Open(".")
	if err != nil {
		return err
	}
	names, err := stream.Readdirnames(2)
	stream.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch {
	case len(names) == 0:
		return nil // The marker was removed before an interruption.
	case len(names) > 1 || names[0] != namespaceMarker:
		return ErrTempNamespaceInUse
	case !validNamespaceMarker(directory, record.Owner, record.ID):
		return ErrTempIdentity
	}
	return directory.Remove(namespaceMarker)
}
