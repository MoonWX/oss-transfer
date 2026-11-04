//go:build darwin

package transferstore

import (
	"os"

	"golang.org/x/sys/unix"
)

// orderedFileSync runs before a write whose durability is deferred to
// Local.Sync is published. Go's File.Sync is F_FULLFSYNC on darwin, which
// flushes the whole drive cache for every file. F_BARRIERFSYNC writes the file
// like fsync(2) and orders it before every later write, so the publishing link
// or rename can never reach stable storage ahead of the payload. Local.Sync
// then issues F_FULLFSYNC once per touched directory, which makes the whole
// batch durable.
func orderedFileSync(file *os.File) error {
	if _, err := unix.FcntlInt(file.Fd(), unix.F_BARRIERFSYNC, 0); err == nil {
		return nil
	}
	// A filesystem without barrier support gets the full flush.
	return file.Sync()
}
