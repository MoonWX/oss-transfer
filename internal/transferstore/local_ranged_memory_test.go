package transferstore

import (
	"context"
	"io"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
)

// lastChunkGate holds an object's final chunk inside Write until release
// closes. An object writes its chunks from one goroutine, in order.
type lastChunkGate struct {
	writes, last int
	reached      func()
	release      <-chan struct{}
}

func (g *lastChunkGate) Write(p []byte) (int, error) {
	g.writes++
	if g.writes == g.last {
		g.reached()
		<-g.release
	}
	return len(p), nil
}

// onesReader fills every read; a range of it is cut off by io.LimitReader.
type onesReader struct{}

func (onesReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 1
	}
	return len(p), nil
}

// A chunk is dropped once it has been written: while many objects are in
// flight, the chunk memory that exists is the memory the budget counts.
// Buffers kept for reuse per object stayed allocated after the budget had
// counted them free, so the default 128 objects in flight held several GiB
// beside a 256 MiB budget.
func TestLocalRangedWriteKeepsNoChunkOnceItIsWritten(t *testing.T) {
	const (
		objects = 32
		chunk   = 1 << 20
		chunks  = 8
		slots   = 4
	)
	budget, err := NewUploadMemoryBudget(objects * slots * chunk)
	if err != nil {
		t.Fatal(err)
	}
	// Garbage is collected only where this test asks, so what one collection
	// leaves is what the writes still hold.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	ranges := func(_ context.Context, _, length int64) (io.ReadCloser, error) {
		return io.NopCloser(io.LimitReader(onesReader{}, length)), nil
	}
	var reached, writers sync.WaitGroup
	reached.Add(objects)
	release := make(chan struct{})
	failures := make(chan error, objects)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for range objects {
		writers.Add(1)
		go func() {
			defer writers.Done()
			// A write that fails before its last chunk must not leave the
			// test waiting for it.
			var once sync.Once
			arrive := func() { once.Do(reached.Done) }
			defer arrive()
			local := &Local{}
			if err := local.SetRangedWrites(LocalRangedOptions{MinSize: 1, ChunkSize: chunk, PartConcurrency: slots, Memory: budget}); err != nil {
				failures <- err
				return
			}
			gate := &lastChunkGate{last: chunks, reached: arrive, release: release}
			failures <- local.writeRanges(context.Background(), gate, ranges, chunks*chunk, nil)
		}()
	}
	reached.Wait()
	// Every object now holds exactly its last chunk, in its writer; all the
	// chunks before it have been written.
	used, waiting := memoryState(budget)
	runtime.GC()
	var held runtime.MemStats
	runtime.ReadMemStats(&held)
	close(release)
	writers.Wait()
	// Memory an earlier test left behind can survive the first collection
	// and is gone by now; it only ever shrinks, so the smaller reading is
	// the baseline. Two collections also empty any pool.
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if used != objects*chunk || waiting != 0 {
		t.Fatalf("budget counts %d bytes in %d objects, %d waiting; want one %d-byte chunk each", used, objects, waiting, chunk)
	}
	// The chunks in the writers are alive; a second set of that size is not.
	if retained := int64(held.HeapAlloc) - int64(min(before.HeapAlloc, after.HeapAlloc)); retained > 2*objects*chunk {
		t.Fatalf("%d MiB of heap held for %d MiB of chunks in flight", retained>>20, used>>20)
	}
	if left, _ := memoryState(budget); left != 0 {
		t.Fatalf("budget still counts %d bytes after every write ended", left)
	}
}
