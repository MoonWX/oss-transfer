//go:build linux

package transferstore

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOrderedFileSyncFallsBackOutsideJournaledFilesystems(t *testing.T) {
	dirs := []string{t.TempDir()}
	if extra := os.Getenv("OSS_TRANSFER_BENCH_DIR"); extra != "" {
		dirs = append(dirs, extra)
	}
	for _, dir := range dirs {
		file, err := os.CreateTemp(dir, "ordered-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(file.Name()) })
		if _, err := file.WriteString("payload"); err != nil {
			t.Fatal(err)
		}
		if err := orderedFileSync(file); err != nil {
			t.Fatal(err)
		}
		var fs unix.Statfs_t
		if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
			t.Fatal(err)
		}
		journaled := journaledFilesystem(int(file.Fd()))
		if journaled && fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.XFS_SUPER_MAGIC {
			t.Fatalf("%s: filesystem %#x treated as journaled", filepath.Dir(file.Name()), fs.Type)
		}
		t.Logf("%s: filesystem %#x, journaled=%v", dir, fs.Type, journaled)
		file.Close()
	}
}
