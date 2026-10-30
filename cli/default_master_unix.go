//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func trustedDiscoveryFile(path string, info os.FileInfo) error {
	for current := path; ; current = filepath.Dir(current) {
		if current != path {
			var err error
			info, err = os.Lstat(current)
			if err != nil {
				return err
			}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 || (stat.Uid != 0 && int(stat.Uid) != os.Geteuid()) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return cliErrorf("集群连接文件权限不安全: %s", path)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
