//go:build !linux && !darwin

package legacyimport

import "errors"

func publishDirectory(from, to string) error {
	return errors.New("atomic migration publication is supported on Linux and macOS")
}
