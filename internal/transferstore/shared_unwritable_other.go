//go:build !windows

package transferstore

func platformUnwritable(error) bool { return false }

// windowsNames is set where file names follow Windows rules.
const windowsNames = false
