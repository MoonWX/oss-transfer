//go:build linux

package transferstore

import (
	"errors"
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
		if err := orderedFileSync(file, &orderingCache{}); err != nil {
			t.Fatal(err)
		}
		var fs unix.Statfs_t
		if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
			t.Fatal(err)
		}
		journaled := writebackOrdersPublication(int(file.Fd()), &orderingCache{})
		if journaled && fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.XFS_SUPER_MAGIC {
			t.Fatalf("%s: filesystem %#x treated as journaled", filepath.Dir(file.Name()), fs.Type)
		}
		t.Logf("%s: filesystem %#x, journaled=%v", dir, fs.Type, journaled)
		file.Close()
	}
}

func TestExt4FastPathRequiresJournalAndSafeOptions(t *testing.T) {
	link, glob, read := readSysfsLink, globProc, readProcFile
	t.Cleanup(func() { readSysfsLink, globProc, readProcFile = link, glob, read })
	readSysfsLink = func(string) (string, error) { return "../../devices/virtual/block/vdz1", nil }
	journal := true
	globProc = func(pattern string) ([]string, error) {
		if pattern != "/proc/fs/jbd2/vdz1-*" {
			t.Errorf("unexpected journal lookup %q", pattern)
		}
		if journal {
			return []string{"/proc/fs/jbd2/vdz1-8"}, nil
		}
		return nil, nil
	}
	options := ""
	readProcFile = func(name string) ([]byte, error) {
		if name != "/proc/fs/ext4/vdz1/options" {
			return nil, errors.New("unexpected file " + name)
		}
		return []byte(options), nil
	}
	for _, tc := range []struct {
		journal bool
		options string
		want    bool
	}{
		{true, "rw\nbarrier\ndata=ordered\n", true},
		{true, "rw\ndata=journal\n", true},
		{false, "rw\ndata=ordered\n", false},
		{true, "rw\ndata=writeback\n", false},
		{true, "rw\njournal_async_commit\ndata=journal\n", false},
		{true, "rw\nnobarrier\ndata=ordered\n", false},
		{true, "rw\n", false},
	} {
		journal, options = tc.journal, tc.options
		if got := ext4OrdersWriteback(unix.Mkdev(253, 1)); got != tc.want {
			t.Errorf("journal=%v options=%q: got %v want %v", tc.journal, tc.options, got, tc.want)
		}
	}
}

// The ext4 decision is cached per Local only: a new Local rereads the mount
// options, so a remount with other options or a replaced filesystem is seen.
func TestExt4DecisionIsCachedPerLocal(t *testing.T) {
	dir := os.Getenv("OSS_TRANSFER_BENCH_DIR")
	if dir == "" {
		t.Skip("set OSS_TRANSFER_BENCH_DIR to an ext4 directory")
	}
	file, err := os.CreateTemp(dir, "cache-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("not ext4")
	}
	read := readProcFile
	t.Cleanup(func() { readProcFile = read })
	reads := 0
	readProcFile = func(name string) ([]byte, error) { reads++; return read(name) }
	first := &orderingCache{}
	writebackOrdersPublication(int(file.Fd()), first)
	writebackOrdersPublication(int(file.Fd()), first)
	if reads != 1 {
		t.Fatalf("one Local read the mount options %d times", reads)
	}
	writebackOrdersPublication(int(file.Fd()), &orderingCache{})
	if reads != 2 {
		t.Fatalf("a new Local reused another Local's decision")
	}
}
