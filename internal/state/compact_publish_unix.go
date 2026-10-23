//go:build !windows

package state

import (
	"os"
	"path/filepath"
)

func publishCompact(temporary, destination string) error {
	// Link is atomic and refuses an existing destination, unlike Rename.
	if err := os.Link(temporary, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	if err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}
