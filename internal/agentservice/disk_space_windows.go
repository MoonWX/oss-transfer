//go:build windows

package agentservice

import (
	"math"

	"golang.org/x/sys/windows"
)

// diskSpace reports the bytes available to this process and the size of the
// volume holding path.
func diskSpace(path string) (free, total int64, ok bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var available, size, all uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &size, &all); err != nil || available > math.MaxInt64 || size > math.MaxInt64 {
		return 0, 0, false
	}
	return int64(available), int64(size), true
}
