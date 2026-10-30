//go:build !windows

package main

import "os"

func privateLocalAuthFile(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return os.ErrPermission
	}
	return nil
}
