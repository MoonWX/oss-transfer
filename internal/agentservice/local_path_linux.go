//go:build linux

package agentservice

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// A stat of the target alone cannot bypass an NFS negative dentry. Refresh
// each parent before resolving its child, so a directory created by another
// client is visible immediately. This is only used by the explicit preflight;
// discovery and transfer keep their normal filesystem caching.
func statPreflightPath(ctx context.Context, path string) (os.FileInfo, error) {
	if err := refreshPreflightPath(ctx, path, 0, syncPreflightAttributes); err != nil && !preflightStatxUnsupported(err) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Stat(path)
}

func preflightStatxUnsupported(err error) bool {
	// Older kernels and container syscall policies may not expose statx. The
	// ordinary stat still enforces filesystem permissions in that case.
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM)
}

func syncPreflightAttributes(path string) error {
	var result unix.Statx_t
	// MODE requests fresh attributes without the timestamp writeback that can
	// accompany a request for MTIME/CTIME on a large active NFS file.
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_FORCE_SYNC|unix.AT_NO_AUTOMOUNT, unix.STATX_MODE, &result)
	if err != nil {
		return &os.PathError{Op: "statx", Path: path, Err: err}
	}
	return nil
}

func refreshPreflightPath(ctx context.Context, path string, links int, syncAttributes func(string) error) error {
	if links > 40 {
		return &os.PathError{Op: "stat", Path: path, Err: unix.ELOOP}
	}
	parts, parent := strings.Split(path, "/"), "/"
	for i, part := range parts {
		if part == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncAttributes(parent); err != nil {
			return err
		}
		next := parent + "/" + part
		info, err := os.Lstat(next)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(next)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(target, "/") {
				target = parent + "/" + target
			}
			if i+1 < len(parts) {
				target += "/" + strings.Join(parts[i+1:], "/")
			}
			return refreshPreflightPath(ctx, target, links+1, syncAttributes)
		}
		// Keep .. components intact: cleaning a path before resolving symlinks
		// changes which directory the kernel will access.
		parent = next
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncAttributes(parent)
}
