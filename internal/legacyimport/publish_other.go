//go:build !linux && !darwin

package legacyimport

func publishDirectory(from, to string) error {
	return errUnsupportedPublication
}
