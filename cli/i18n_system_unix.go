//go:build !windows

package main

// Unix shells supply their message locale through LC_ALL, LC_MESSAGES or LANG.
func platformLocaleName() string { return "" }
