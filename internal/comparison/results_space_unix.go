//go:build !windows

package comparison

import "golang.org/x/sys/unix"

// freeDiskBytes is the space available to this process on the file system
// holding directory (statfs f_bavail × f_bsize).
func freeDiskBytes(directory string) (uint64, error) {
	var info unix.Statfs_t
	if err := unix.Statfs(directory, &info); err != nil {
		return 0, err
	}
	if info.Bavail < 0 || info.Bsize <= 0 {
		return 0, nil
	}
	return uint64(info.Bavail) * uint64(info.Bsize), nil
}
