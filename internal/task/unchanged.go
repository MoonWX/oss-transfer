package task

import "errors"

// Return this only before any mutation. bbolt's normal Update rolls back the
// read-only decision instead of writing and syncing another metadata page.
var errUnchanged = errors.New("task state is unchanged")

func unchangedOK(err error) error {
	if err == errUnchanged {
		return nil
	}
	return err
}
