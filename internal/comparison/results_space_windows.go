//go:build windows

package comparison

import "golang.org/x/sys/windows"

// freeDiskBytes is the space available to this process's user on the volume
// holding directory.
func freeDiskBytes(directory string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	err = windows.GetDiskFreeSpaceEx(path, &available, &total, &free)
	return available, err
}
