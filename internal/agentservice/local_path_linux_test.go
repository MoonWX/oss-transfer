//go:build linux

package agentservice

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPreflightFreshPathPreservesSymlinksAndParentComponents(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"actual", "actual/nested", "actual/target", "target"} {
		if err := os.Mkdir(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("actual/nested", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	path := root + "/link/../target"
	info, err := statPreflightPath(context.Background(), path)
	want, wantErr := os.Stat(filepath.Join(root, "actual/target"))
	if err != nil || wantErr != nil || !os.SameFile(info, want) {
		t.Fatal("fresh stat changed symlink/.. semantics", info, err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := statPreflightPath(context.Background(), filepath.Join(root, "dangling")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dangling symlink was accepted", err)
	}
	if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, err := statPreflightPath(context.Background(), filepath.Join(root, "loop")); !errors.Is(err, unix.ELOOP) {
		t.Fatal("symlink loop did not stop", err)
	}
}

func TestPreflightFreshPathPreservesErrorsAndCancellation(t *testing.T) {
	for _, failure := range []error{unix.EACCES, unix.EIO, unix.ENOTDIR} {
		err := refreshPreflightPath(context.Background(), t.TempDir(), 0, func(string) error { return failure })
		if !errors.Is(err, failure) || preflightStatxUnsupported(err) {
			t.Fatal("filesystem error was hidden by compatibility fallback", failure, err)
		}
	}
	for _, failure := range []error{unix.ENOSYS, unix.EOPNOTSUPP, unix.EPERM, unix.EINVAL} {
		if !preflightStatxUnsupported(&os.PathError{Op: "statx", Path: "/", Err: failure}) {
			t.Fatal("unsupported statx did not allow ordinary stat", failure)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := refreshPreflightPath(ctx, t.TempDir(), 0, func(string) error { t.Fatal("cancelled preflight accessed storage"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was lost", err)
	}
}

// Run this test binary on one NFS client, then create the named directory on
// another client when it prints READY. It requires a real stale lookup before
// verifying the fix, so a local filesystem cannot produce a false pass.
func TestPreflightFreshPathOnNFS(t *testing.T) {
	path := os.Getenv("OSS_TRANSFER_PREFLIGHT_NFS_PATH")
	if path == "" {
		t.Skip("requires coordinated directory creation on another NFS client")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("test directory must not exist before the remote mkdir", err)
	}
	fmt.Println("NFS_PREFLIGHT_READY")
	if text, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil || text != "created\n" {
		t.Fatal("remote mkdir was not confirmed", text, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("NFS negative lookup was not reproduced", err)
	}
	fmt.Println("ordinary stat still reports ENOENT after remote mkdir")
	if err := syncPreflightAttributes(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target-only FORCE_SYNC unexpectedly bypassed negative lookup", err)
	}
	fmt.Println("target-only FORCE_SYNC still reports ENOENT")
	info, err := statPreflightPath(context.Background(), path)
	if err != nil || !info.IsDir() {
		t.Fatal("preflight did not see the remotely created directory", err)
	}
	fmt.Println("preflight parent refresh sees DIRECTORY")
}
