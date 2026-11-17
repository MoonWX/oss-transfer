package transferstore

import (
	"errors"
	"syscall"
)

// ERROR_WRITE_PROTECT is what Windows returns for write-protected media and
// some read-only shares; syscall does not name it.
const errorWriteProtect = syscall.Errno(19)

func platformReadOnly(err error) bool { return errors.Is(err, errorWriteProtect) }
