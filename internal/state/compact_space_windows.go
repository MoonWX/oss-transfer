//go:build windows

package state

import "golang.org/x/sys/windows"

func compactFreeBytes(directory string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	err = windows.GetDiskFreeSpaceEx(path, &available, &total, &free)
	return available, err
}
