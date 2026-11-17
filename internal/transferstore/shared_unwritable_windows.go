package transferstore

import (
	"errors"
	"syscall"
)

// Windows errors for media that cannot be written; syscall does not name them.
const (
	errorHandleDiskFull    = syscall.Errno(39)
	errorWriteProtect      = syscall.Errno(19)
	errorDiskFull          = syscall.Errno(112)
	errorDiskQuotaExceeded = syscall.Errno(1295)
)

func platformUnwritable(err error) bool {
	return errors.Is(err, errorWriteProtect) || errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull) || errors.Is(err, errorDiskQuotaExceeded)
}

// windowsNames is set where file names follow Windows rules.
const windowsNames = true
