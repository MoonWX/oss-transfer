package transferstore

import (
	"errors"
	"io/fs"
	"syscall"
)

// IsReadOnlyStorage reports whether a write failed only because this Agent
// may not write there: permission denied (EACCES, EPERM, Windows access
// denied) or a read-only file system (EROFS, Windows write-protected media).
// Such a root can still be a valid upload source.
func IsReadOnlyStorage(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) || platformReadOnly(err))
}
