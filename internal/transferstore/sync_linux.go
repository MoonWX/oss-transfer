//go:build linux

package transferstore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// orderedFileSync runs before a write whose durability is deferred to
// Local.Sync is published.
//
// On ext4 with an internal journal in data=ordered or data=journal mode
// (without journal_async_commit), and on XFS for files on the data device,
// waiting for writeback is enough to order the payload before the publishing
// link or rename: writeback allocates the blocks and updates the on-disk size
// in a journal transaction no later than the one that records the new name,
// transactions commit in order, and each commit flushes the device cache
// before its commit record. So a name that survives a crash always refers to
// the complete payload, and the directory fsync in Local.Sync commits all of
// it at once. Every other case (no journal, async commit, XFS realtime files,
// network, FUSE, overlay, anything not confirmed) gets a full fsync here.
func orderedFileSync(file *os.File, cache *orderingCache) error {
	fd := int(file.Fd())
	if !writebackOrdersPublication(fd, cache) {
		return file.Sync()
	}
	return unix.SyncFileRange(fd, 0, 0, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
}

// Test hooks for the kernel interfaces read by the ext4 check.
var (
	readSysfsLink = os.Readlink
	readProcFile  = os.ReadFile
	globProc      = filepath.Glob
)

// The ext4 check is cached per Local (one job), keyed by mount ID, device and
// filesystem ID. A new mount, a reused device number or a cloned image is
// checked again, and every job rereads the mount options. ext4 refuses to
// change the data mode, journal checksums or async commit on remount, and
// removing a journal needs an unmount, so a mount's answer cannot go stale.
type ext4Key struct {
	mount uint64
	dev   uint64
	fsid  unix.Fsid
}

func writebackOrdersPublication(fd int, cache *orderingCache) bool {
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil {
		return false
	}
	switch fs.Type {
	case unix.XFS_SUPER_MAGIC:
		// Realtime files keep data on the rt device, which a log force never
		// flushes; xfs_file_fsync flushes it explicitly.
		realtime, ok := xfsRealtime(fd)
		return ok && !realtime
	case unix.EXT4_SUPER_MAGIC:
	default:
		return false
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return false
	}
	var stx unix.Statx_t
	if unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx) != nil || stx.Mask&unix.STATX_MNT_ID == 0 {
		return false // Without a mount ID the cached answer cannot be tied to a mount.
	}
	key := ext4Key{mount: stx.Mnt_id, dev: uint64(st.Dev), fsid: fs.Fsid}
	if known, ok := cache.modes.Load(key); ok {
		return known.(bool)
	}
	ordered := ext4OrdersWriteback(uint64(st.Dev))
	cache.modes.Store(key, ordered)
	return ordered
}

// ext4OrdersWriteback confirms an internal jbd2 journal (named
// <block device>-<journal inode>) and safe mount options. ext2 and ext4
// without a journal share the magic number; journal_async_commit writes the
// commit record without flushing the data that preceded it. Anything it cannot
// confirm, such as an external journal, counts as unsafe.
func ext4OrdersWriteback(dev uint64) bool {
	link, err := readSysfsLink(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(dev), unix.Minor(dev)))
	if err != nil {
		return false
	}
	name := filepath.Base(link)
	matches, err := globProc(filepath.Join("/proc/fs/jbd2", name+"-*"))
	if err != nil || len(matches) == 0 {
		return false
	}
	options, err := readProcFile(filepath.Join("/proc/fs/ext4", name, "options"))
	if err != nil {
		return false
	}
	ordered := false
	for _, option := range strings.Fields(string(options)) {
		switch option {
		case "data=ordered", "data=journal":
			ordered = true
		case "journal_async_commit", "nobarrier", "barrier=0":
			return false
		}
	}
	return ordered
}

// fsxattr mirrors struct fsxattr from linux/fs.h.
type fsxattr struct {
	xflags     uint32
	extsize    uint32
	nextents   uint32
	projid     uint32
	cowextsize uint32
	pad        [8]byte
}

const fsXflagRealtime = 0x1

// xfsRealtime reads FS_XFLAG_REALTIME. ok is false where the ioctl number is
// not the generic encoding or the call fails, so callers fall back to fsync.
func xfsRealtime(fd int) (realtime, ok bool) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "386", "arm", "riscv64", "loong64", "s390x":
	default:
		return false, false
	}
	var attr fsxattr
	// _IOR('X', 31, struct fsxattr)
	const fsIocFsgetxattr = 2<<30 | uintptr(unsafe.Sizeof(fsxattr{}))<<16 | 'X'<<8 | 31
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIocFsgetxattr, uintptr(unsafe.Pointer(&attr))); errno != 0 {
		return false, false
	}
	return attr.xflags&fsXflagRealtime != 0, true
}
