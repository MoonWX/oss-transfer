//go:build linux || darwin || freebsd

package agentservice

import (
	"math"

	"golang.org/x/sys/unix"
)

// diskSpace reports the bytes available to this process and the size of the
// file system holding path.
func diskSpace(path string) (free, total int64, ok bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	block := uint64(stat.Bsize)
	available, blocks := uint64(stat.Bavail), uint64(stat.Blocks)
	if block == 0 || available > math.MaxInt64/block || blocks > math.MaxInt64/block {
		return 0, 0, false
	}
	return int64(available * block), int64(blocks * block), true
}
