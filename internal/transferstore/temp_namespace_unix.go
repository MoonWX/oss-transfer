//go:build !windows

package transferstore

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func createTempNamespace(parent *os.Root, name string) error { return parent.Mkdir(name, 0700) }

func secureTempNamespace(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Chmod(0700)
}
func verifyTempNamespace(root *os.Root) error {
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrTempIdentity
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return ErrTempIdentity
	}
	return nil
}

func syncTempNamespace(root *os.Root) error { return syncPublished(root, ".") }

// The directory descriptors pin both names independently. A replacement of the
// visible staging-directory path cannot substitute a different payload.
func publishTempAcrossRoots(source, destination *os.Root, name, target string, overwrite bool, expectedIdentity string) error {
	if filepath.Base(name) != name || filepath.Base(target) != target || name == "." || target == "." || name == ".." || target == ".." || name == "" || target == "" || expectedIdentity == "" {
		return ErrTempIdentity
	}
	info, err := source.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || fileIdentity(info) != expectedIdentity {
		return ErrTempIdentity
	}
	from, err := source.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := destination.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	if overwrite {
		return unix.Renameat(int(from.Fd()), name, int(to.Fd()), target)
	}
	return unix.Linkat(int(from.Fd()), name, int(to.Fd()), target, 0)
}
