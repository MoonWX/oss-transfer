//go:build !windows

package main

import (
	"os"
)

func protectSessionPath(path string, directory bool) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return os.Chmod(path, mode)
}

func checkSessionPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return cliErrorf("登录会话文件权限应为 0600: %s", path)
	}
	return nil
}
