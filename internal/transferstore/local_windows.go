//go:build windows

package transferstore

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func platformKey(key string) error {
	for _, component := range strings.Split(key, "/") {
		// Refuse ADS and names Windows aliases by trimming suffix characters.
		// Otherwise distinct S3 keys can unexpectedly overwrite the same file.
		if strings.ContainsAny(component, ":<>\"|?*") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return ErrInvalidKey
		}
		for _, r := range component {
			if r < 32 {
				return ErrInvalidKey
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
			return ErrInvalidKey
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return ErrInvalidKey
		}
	}
	return nil
}

func handleIdentity(file *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("/%d/%d/%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func openedIdentity(file *os.File, _ fs.FileInfo) (string, error) { return handleIdentity(file) }

func storageIdentity(root *os.Root, key string, expected fs.FileInfo) (string, error) {
	file, err := root.Open(key)
	if err != nil {
		return "", err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(expected, actual) {
		return "", ErrSourceChanged
	}
	return handleIdentity(file)
}

func storageObject(root *os.Root, key string, info fs.FileInfo) (Object, error) {
	identity, err := storageIdentity(root, key, info)
	if err != nil {
		return Object{}, err
	}
	object := localObject(key, info)
	object.ETag = identityFingerprint(info, identity)
	return object, nil
}

func openedObject(key string, file *os.File, info fs.FileInfo) (Object, error) {
	identity, err := handleIdentity(file)
	if err != nil {
		return Object{}, err
	}
	object := localObject(key, info)
	object.ETag = identityFingerprint(info, identity)
	return object, nil
}

// Windows does not expose POSIX directory fsync. os.Root.Rename uses a confined
// NtSetInformationFile rename with replace-if-exists semantics. Flush the final
// writable file handle as well as the temporary file before publishing. Namespace
// survival after power loss still depends on the filesystem's journal; recovery
// must revalidate the completed object instead of treating a ledger as proof.
func syncPublished(directory *os.Root, name string) error {
	file, err := directory.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
