package transferstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Linux reports a writeback error to only one fsync per open file
// description (errseq): a second fsync of the same handle returns nil. A
// directory fsync that fails must therefore fail the barrier that covers it,
// even when the entry is evicted from the cache and a Sync runs meanwhile.
func TestLocalEvictedDirectoryFsyncFailureFailsTheBarrier(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	root := t.TempDir()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	write := func(key string) {
		if _, err := local.Write(context.Background(), key, bytes.NewReader([]byte("x")), WriteOptions{Size: 1, DeferDurability: true}); err != nil {
			t.Error(err)
		}
	}
	write("d000/f")
	victim, err := os.Stat(filepath.Join(root, "d000"))
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	original := syncDirectory
	t.Cleanup(func() { syncDirectory = original })
	syncDirectory = func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr == nil && os.SameFile(info, victim) {
			first := false
			once.Do(func() { first = true })
			if first {
				close(started)
				<-release
				return errors.New("EIO")
			}
			return nil // errseq: the error was reported once already
		}
		return original(file)
	}
	// Push d000 out of the directory cache.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= maxCachedDirs; i++ {
			write(fmt.Sprintf("d%03d/f", i))
		}
	}()
	select {
	case <-started:
		// An implementation that fsyncs at eviction: run a barrier while
		// that fsync is still in flight.
		result := make(chan error, 1)
		go func() { result <- local.Sync(context.Background()) }()
		err = <-result
		close(release)
		<-done
	case <-done:
		// Eviction left the fsync to the barrier.
		close(release)
		err = local.Sync(context.Background())
	}
	if err == nil {
		t.Fatal("barrier succeeded although the directory fsync covering d000/f failed")
	}
	if local.Sync(context.Background()) == nil {
		t.Fatal("a later barrier succeeded after the failure")
	}
}
