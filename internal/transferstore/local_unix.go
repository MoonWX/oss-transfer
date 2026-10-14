//go:build !windows

package transferstore

import (
	"io/fs"
	"os"
)

func platformKey(string) error { return nil }

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
