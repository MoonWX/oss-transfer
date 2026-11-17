//go:build !windows

package transferstore

func platformReadOnly(error) bool { return false }
