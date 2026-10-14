package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// PowerShell and cmd do not normally export LANG. Read the user's Windows
// locale directly rather than starting a shell for every CLI invocation.
func platformLocaleName() string {
	var name [85]uint16 // LOCALE_NAME_MAX_LENGTH
	getLocale := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultLocaleName")
	if err := getLocale.Find(); err != nil {
		return ""
	}
	n, _, _ := getLocale.Call(uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(name[:])
}
