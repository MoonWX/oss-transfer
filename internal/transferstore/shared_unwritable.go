package transferstore

import (
	"errors"
	"io/fs"
	"syscall"
)

// IsUnwritableStorage reports whether a write failed only because this Agent
// cannot write there: permission denied (EACCES, EPERM, Windows access
// denied), a read-only file system (EROFS, Windows write-protected media) or
// no space left (ENOSPC, EDQUOT, Windows disk full or quota exceeded). Such a
// root can still be a valid upload source, so the Master may compare sampled
// files instead of the shared test file.
func IsUnwritableStorage(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || platformUnwritable(err))
}
