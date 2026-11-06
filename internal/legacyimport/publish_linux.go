//go:build linux

package legacyimport

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var renameNoReplace = func(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

func publishDirectory(from, to string) error {
	err := renameNoReplace(from, to)
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	// Early CentOS / RHEL 7 kernels (3.10.0-123 in 7.0) lack renameat2, and
	// some file systems reject RENAME_NOREPLACE. rename(2) replaces only an empty
	// directory, so creating the destination exclusively first still never
	// replaces existing data. (os.Rename refuses any existing directory.)
	if err := os.Mkdir(to, 0700); err != nil {
		return err
	}
	if err := unix.Rename(from, to); err != nil {
		_ = os.Remove(to)
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
