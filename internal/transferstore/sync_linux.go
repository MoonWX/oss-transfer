//go:build linux

package transferstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// orderedFileSync runs before a write whose durability is deferred to
// Local.Sync is published.
//
// On a journaled ext4 or XFS filesystem, waiting for writeback is enough to
// order the payload before the publishing link or rename: writeback allocates
// the blocks and updates the on-disk size in a journal transaction no later
// than the one that records the new name, transactions commit in order, and
// each commit flushes the device cache after the data writes completed. So a
// name that survives a crash always refers to the complete payload, and the
// directory fsync in Local.Sync commits all of it at once. Every other
// filesystem (no journal, network, FUSE, overlay) gets a full fsync here.
func orderedFileSync(file *os.File) error {
	fd := int(file.Fd())
	if !journaledFilesystem(fd) {
		return file.Sync()
	}
	return unix.SyncFileRange(fd, 0, 0, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
}

var ext4Journals sync.Map // device number -> bool

func journaledFilesystem(fd int) bool {
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil {
		return false
	}
	switch fs.Type {
	case unix.XFS_SUPER_MAGIC:
		return true // XFS always logs metadata.
	case unix.EXT4_SUPER_MAGIC:
	default:
		return false
	}
	// ext2 and ext4 without a journal share this magic number.
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return false
	}
	if known, ok := ext4Journals.Load(st.Dev); ok {
		return known.(bool)
	}
	journaled := ext4HasJournal(st.Dev)
	ext4Journals.Store(st.Dev, journaled)
	return journaled
}

// ext4HasJournal reports whether jbd2 runs an internal journal for the
// device; the kernel names it <block device>-<journal inode>. Anything it
// cannot confirm, such as an external journal, counts as no journal.
func ext4HasJournal(dev uint64) bool {
	link, err := os.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(dev), unix.Minor(dev)))
	if err != nil {
		return false
	}
	matches, err := filepath.Glob(filepath.Join("/proc/fs/jbd2", filepath.Base(link)+"-*"))
	return err == nil && len(matches) > 0
}
