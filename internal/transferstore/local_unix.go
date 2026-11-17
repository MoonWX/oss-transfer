//go:build !windows

package transferstore

import (
	"io/fs"
	"os"
	"syscall"
)

func platformKey(string) error { return nil }

// localIdentityOpens reports whether a file's identity needs an open handle.
// On Unix the device and inode come with the lookup itself.
const localIdentityOpens = false

func storageIdentity(_ *os.Root, _ string, info fs.FileInfo) (string, error) {
	return fileIdentity(info), nil
}
func storageObject(_ *os.Root, key string, info fs.FileInfo) (Object, error) {
	return localObject(key, info), nil
}
func openedObject(key string, _ *os.File, info fs.FileInfo) (Object, error) {
	return localObject(key, info), nil
}
func openedIdentity(_ *os.File, info fs.FileInfo) (string, error) {
	return fileIdentity(info), nil
}
func syncPublished(directory *os.Root, _ string) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// lookupIdentity is what os.SameFile compares of a file a lookup found, kept
// without the rest of the lookup: on Unix, its device and inode.
type lookupIdentity struct {
	dev, ino uint64
	known    bool
}

func identityOf(info fs.FileInfo) lookupIdentity {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return lookupIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino), known: true}
	}
	return lookupIdentity{}
}

// same reports whether info is the file the identity was taken from, as
// os.SameFile does.
func (id lookupIdentity) same(info fs.FileInfo) bool {
	other := identityOf(info)
	return id.known && other.known && id.dev == other.dev && id.ino == other.ino
}
