//go:build !windows

package servicehost

import (
	"os"
	"syscall"
)

func privateMaintenanceFile(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().Perm()&0077 != 0 || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return os.ErrPermission
	}
	return nil
}
