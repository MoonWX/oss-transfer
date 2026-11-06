package transferstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// gatedRange blocks its first read until gate closes, and reports EOF.
type gatedRange struct {
	io.ReadCloser
	ctx  context.Context
	gate <-chan struct{}
	eof  func()
	open bool
	done bool
}

func (r *gatedRange) Read(p []byte) (int, error) {
	if !r.open {
		select {
		case <-r.gate:
			r.open = true
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !r.done && r.eof != nil {
		r.done = true
		r.eof()
	}
	return n, err
}

var closedGate = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func tempFileSizes(t *testing.T, root string) []int64 {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int64
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, info.Size())
		}
	}
	return sizes
}

// Chunks that arrive ahead of a stalled first chunk wait in memory: nothing
// is written past the gap, and no chunk beyond the window is fetched.
func TestLocalRangedWriteHoldsLaterChunksBehindTheGap(t *testing.T) {
	const base = 4
	size := int64(9*testRangePart + 5)
	budget, _ := NewUploadMemoryBudget(64 << 20)
	s, root, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: base, Memory: budget})
	data := randomPayload(29, size)
	harness := newRangeHarness(t, data, 29)
	gate := make(chan struct{})
	var ahead sync.WaitGroup
	ahead.Add(base - 1)
	harness.hook = func(ctx context.Context, offset, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		switch {
		case offset == 0:
			return &gatedRange{ReadCloser: reader, ctx: ctx, gate: gate}, nil
		case offset < base*testRangePart:
			return &gatedRange{ReadCloser: reader, ctx: ctx, gate: closedGate, eof: ahead.Done}, nil
		}
		return reader, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges})
		done <- err
	}()
	ahead.Wait()
	if sizes := tempFileSizes(t, root); len(sizes) != 1 || sizes[0] != 0 {
		t.Fatalf("data written past the missing first chunk: %v", sizes)
	}
	if used, _ := memoryState(budget); used != base*testRangePart {
		t.Fatalf("window holds %d bytes, want %d", used, base*testRangePart)
	}
	if open, _, _ := harness.state(); open != base {
		t.Fatalf("fetched %d chunks with a window of %d", open, base)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 {
		t.Fatalf("memory used=%d waiting=%d after the write", used, waiting)
	}
}

// recordingWriter records each sequential write; block, when set, holds the
// first write until it closes. failAt makes that write (1-based) fail.
type recordingWriter struct {
	mu      sync.Mutex
	data    bytes.Buffer
	writes  []int
	entered chan struct{}
	block   chan struct{}
	failAt  int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	call := len(w.writes)
	w.writes = append(w.writes, len(p))
	w.mu.Unlock()
	if call == 0 && w.block != nil {
		close(w.entered)
		<-w.block
	}
	if w.failAt > 0 && call+1 == w.failAt {
		return 0, errors.New("no space left on device")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}

// A writer that falls behind fills the window; fetching stops until it
// catches up, and the file still receives every chunk in order.
func TestLocalRangedWriteBackpressureWhenWriterStalls(t *testing.T) {
	const base = 3
	size := int64(10*testRangePart - 1)
	budget, _ := NewUploadMemoryBudget(64 << 20)
	s, _, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: base, Memory: budget})
	data := randomPayload(31, size)
	harness := newRangeHarness(t, data, 31)
	var fetched sync.WaitGroup
	fetched.Add(base)
	var count atomic.Int64
	harness.hook = func(ctx context.Context, _, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		if count.Add(1) <= base {
			return &gatedRange{ReadCloser: reader, ctx: ctx, gate: closedGate, eof: fetched.Done}, nil
		}
		return reader, nil
	}
	writer := &recordingWriter{entered: make(chan struct{}), block: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.writeRanges(context.Background(), writer, harness.ranges, size, maxPartSlots) }()
	<-writer.entered
	fetched.Wait()
	for range 3 { // the stalled writer keeps every slot: no further fetch
		if open, _, _ := harness.state(); open != base {
			t.Fatalf("fetched %d chunks while the writer stalled, window %d", open, base)
		}
		if used, _ := memoryState(budget); used != base*testRangePart {
			t.Fatalf("window holds %d bytes", used)
		}
		runtime.Gosched()
	}
	close(writer.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.data.Bytes(), data) || len(writer.writes) != 10 {
		t.Fatalf("writes %v, equal=%v", writer.writes, bytes.Equal(writer.data.Bytes(), data))
	}
	if used, _ := memoryState(budget); used != 0 {
		t.Fatal("memory not released", used)
	}
}

// The memory budget, not the slot count, bounds a window it cannot cover:
// no more chunks are fetched at once than fit in memory, borrowed ones too.
func TestLocalRangedWriteStaysWithinTheMemoryBudget(t *testing.T) {
	budget, _ := NewUploadMemoryBudget(2 * testRangePart)
	lender := NewPartLender()
	lender.SetIdle(func() int { return 100 })
	s, root, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 4, PartLender: lender, Memory: budget})
	size := int64(12 * testRangePart)
	data := randomPayload(37, size)
	harness := newRangeHarness(t, data, 37)
	var peak atomic.Int64
	harness.hook = func(_ context.Context, _, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		used, _ := memoryState(budget)
		for old := peak.Load(); used > old && !peak.CompareAndSwap(old, used); old = peak.Load() {
		}
		return reader, nil
	}
	if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if _, _, open := harness.state(); open > 2 || peak.Load() > 2*testRangePart {
		t.Fatalf("peak %d concurrent chunks, %d bytes, budget %d", open, peak.Load(), 2*testRangePart)
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 || lender.Lent() != 0 {
		t.Fatalf("used=%d waiting=%d lent=%d", used, waiting, lender.Lent())
	}
}

// A write error fails the object, stops every fetch and frees the window.
func TestLocalRangedWriteFailsOnWriterError(t *testing.T) {
	budget, _ := NewUploadMemoryBudget(64 << 20)
	lender := NewPartLender()
	lender.SetIdle(func() int { return 100 })
	s, _, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 4, PartLender: lender, Memory: budget})
	size := int64(20 * testRangePart)
	harness := newRangeHarness(t, randomPayload(41, size), 41)
	writer := &recordingWriter{failAt: 2}
	err := s.writeRanges(context.Background(), writer, harness.ranges, size, maxPartSlots)
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatal(err)
	}
	if _, active, _ := harness.state(); active != 0 {
		t.Fatal("range readers left open", active)
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 || lender.Lent() != 0 {
		t.Fatalf("used=%d waiting=%d lent=%d", used, waiting, lender.Lent())
	}
}
