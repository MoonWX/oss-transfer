//go:build !windows

package state

import (
	"os"
	"syscall"
)

func checkPrivateCredentialKey(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().Perm()&0077 != 0 || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return os.ErrPermission
	}
	return nil
}
func protectCredentialKey(file *os.File) error { return file.Chmod(0600) }
