package transferstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testRangePart = 64 << 10

// rangeRegistry is a durable-registry fake. It records whether the temporary
// file was still empty when registered and whether it was forgotten.
type rangeRegistry struct {
	mu         sync.Mutex
	registered map[string]LocalTempRecord
	forgotten  int
	nonEmpty   bool
}

func newRangeRegistry() *rangeRegistry {
	return &rangeRegistry{registered: map[string]LocalTempRecord{}}
}
func (r *rangeRegistry) OwnerID() string { return strings.Repeat("b", 64) }
func (r *rangeRegistry) Register(_ context.Context, record LocalTempRecord) error {
	if err := ValidateLocalTempRecord(record); err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(record.ParentPath, record.Name))
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil || info.Size() != 0 {
		r.nonEmpty = true
	}
	r.registered[record.ID] = record
	return nil
}
func (r *rangeRegistry) Forget(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.registered, id)
	r.forgotten++
	return nil
}
func (r *rangeRegistry) pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.registered)
}

// rangeHarness serves byte ranges of data and records every request. hook,
// when set, may replace the reader of one request.
type rangeHarness struct {
	t      *testing.T
	data   []byte
	rng    *rand.Rand
	mu     sync.Mutex
	calls  map[int64]int // range offset -> requests
	sizes  map[int64]int64
	active int
	peak   int
	open   int
	hook   func(ctx context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error)
	// registry, when set, must have registered the file before any range opens.
	registry *rangeRegistry
}

func newRangeHarness(t *testing.T, data []byte, seed uint64) *rangeHarness {
	return &rangeHarness{t: t, data: data, rng: rand.New(rand.NewPCG(seed, 5)), calls: map[int64]int{}, sizes: map[int64]int64{}}
}

func (h *rangeHarness) ranges(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset+length > int64(len(h.data)) {
		return nil, errors.New("range outside object")
	}
	if h.registry != nil && h.registry.pending() == 0 {
		h.t.Error("range opened before temporary registration")
	}
	h.mu.Lock()
	h.calls[offset]++
	attempt := h.calls[offset]
	h.sizes[offset] = length
	h.active++
	h.open++
	h.peak = max(h.peak, h.active)
	seed := h.rng.Uint64()
	h.mu.Unlock()
	var reader io.ReadCloser = &shortReads{reader: bytes.NewReader(h.data[offset : offset+length]), rng: rand.New(rand.NewPCG(seed, 1))}
	if h.hook != nil {
		var err error
		if reader, err = h.hook(ctx, offset, length, attempt, reader); err != nil {
			h.closed()
			return nil, err
		}
	}
	return &trackedRange{ReadCloser: reader, done: h.closed}, nil
}

func (h *rangeHarness) closed() {
	h.mu.Lock()
	h.active--
	h.mu.Unlock()
}

func (h *rangeHarness) state() (open, active, peak int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open, h.active, h.peak
}

// shortReads returns random short reads and sometimes merges EOF into the
// final data read, as limiters and TLS readers do.
type shortReads struct {
	reader *bytes.Reader
	rng    *rand.Rand
}

func (r *shortReads) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1+r.rng.IntN(len(p))]
	}
	n, err := r.reader.Read(p)
	if err == nil && r.reader.Len() == 0 && r.rng.IntN(2) == 0 {
		err = io.EOF
	}
	return n, err
}
func (r *shortReads) Close() error { return nil }

type trackedRange struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (r *trackedRange) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.done)
	return err
}

func rangedLocal(t *testing.T, options LocalRangedOptions) (*Local, string, *rangeRegistry) {
	t.Helper()
	s, root := localFixture(t)
	if options.MinSize == 0 {
		options.MinSize = 1
	}
	if options.ChunkSize == 0 {
		options.ChunkSize = testRangePart
	}
	if options.Memory == nil {
		budget, err := NewUploadMemoryBudget(64 << 20)
		if err != nil {
			t.Fatal(err)
		}
		options.Memory = budget
	}
	if err := s.SetRangedWrites(options); err != nil {
		t.Fatal(err)
	}
	registry := newRangeRegistry()
	s.SetTempRegistry(registry)
	return s, root, registry
}

func randomPayload(seed uint64, size int64) []byte {
	rng := rand.New(rand.NewPCG(seed, 9))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	return data
}

// unusedInput fails the test if a ranged write reads its streaming input.
type unusedInput struct{ t *testing.T }

func (r unusedInput) Read([]byte) (int, error) {
	r.t.Error("ranged write read the streaming input")
	return 0, errors.New("unused input read")
}

// assertOnlyFiles fails if the directory holds anything but the named files,
// such as a leaked or published partial temporary file.
func assertOnlyFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	sort.Strings(found)
	sort.Strings(names)
	if strings.Join(found, "\x00") != strings.Join(names, "\x00") {
		t.Fatalf("directory holds %q, want %q", found, names)
	}
}

func TestLocalRangedWriteReassemblesEverySize(t *testing.T) {
	sizes := []int64{testRangePart + 1, 2 * testRangePart, 2*testRangePart + 1, 5*testRangePart - 1, 7*testRangePart + 12345, 40*testRangePart + 3}
	for i, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, root, registry := rangedLocal(t, LocalRangedOptions{PartConcurrency: 1 + i%4})
			data := randomPayload(uint64(size), size)
			harness := newRangeHarness(t, data, uint64(i))
			harness.registry = registry
			if !s.RangedWrite(size) {
				t.Fatal("multi-range object not ranged")
			}
			result, err := s.Write(context.Background(), "dir/large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges})
			if err != nil || !result.Committed || result.Object.Size != size {
				t.Fatalf("ranged write: %+v %v", result, err)
			}
			got, err := os.ReadFile(filepath.Join(root, "dir", "large.bin"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("content differs: %v", err)
			}
			count := (size + testRangePart - 1) / testRangePart
			if int64(len(harness.calls)) != count {
				t.Fatalf("requested %d ranges, want %d", len(harness.calls), count)
			}
			var covered int64
			for offset, calls := range harness.calls {
				if calls != 1 || offset%testRangePart != 0 || harness.sizes[offset] != min(testRangePart, size-offset) {
					t.Fatalf("range %d: %d calls of %d bytes", offset, calls, harness.sizes[offset])
				}
				covered += harness.sizes[offset]
			}
			if _, active, _ := harness.state(); covered != size || active != 0 {
				t.Fatalf("covered %d of %d, %d readers left open", covered, size, active)
			}
			if registry.nonEmpty || registry.pending() != 0 || registry.forgotten != 1 {
				t.Fatalf("temporary record: nonEmpty=%v pending=%d forgotten=%d", registry.nonEmpty, registry.pending(), registry.forgotten)
			}
			assertOnlyFiles(t, filepath.Join(root, "dir"), "large.bin")
		})
	}
}

func TestLocalRangedWriteSelection(t *testing.T) {
	s, _ := localFixture(t)
	if s.RangedWrite(10 << 30) {
		t.Fatal("ranged writes selected without a memory budget")
	}
	budget, _ := NewUploadMemoryBudget(DefaultUploadMemoryBytes)
	if err := s.SetRangedWrites(LocalRangedOptions{Memory: budget}); err != nil {
		t.Fatal(err)
	}
	for size, want := range map[int64]bool{0: false, 1 << 20: false, 128<<20 - 1: false, 128 << 20: true, 10 << 30: true} {
		if got := s.RangedWrite(size); got != want {
			t.Errorf("default RangedWrite(%d) = %v", size, got)
		}
	}
	if s.ranged.ChunkSize != 8<<20 || s.ranged.PartConcurrency != 4 {
		t.Fatalf("defaults %+v", s.ranged)
	}
	small, _ := NewUploadMemoryBudget(4 << 20)
	if err := s.SetRangedWrites(LocalRangedOptions{Memory: small}); err != nil || s.RangedWrite(10<<30) {
		t.Fatal("chunks larger than the memory budget still selected", err)
	}
	if err := s.SetRangedWrites(LocalRangedOptions{MinSize: -1, Memory: budget}); err != nil || s.RangedWrite(10<<30) {
		t.Fatal("disabled ranged writes still selected", err)
	}
	for _, concurrency := range []int{-1, 17} {
		if err := s.SetRangedWrites(LocalRangedOptions{PartConcurrency: concurrency}); err == nil {
			t.Fatal("accepted part concurrency", concurrency)
		}
	}
	for _, chunk := range []int64{-1, 65 << 20} {
		if err := s.SetRangedWrites(LocalRangedOptions{ChunkSize: chunk}); err == nil {
			t.Fatal("accepted chunk size", chunk)
		}
	}
	// A streamed write ignores Ranges when the object is a single range.
	s2, root, _ := rangedLocal(t, LocalRangedOptions{})
	result, err := s2.Write(context.Background(), "small", strings.NewReader("small"), WriteOptions{Size: 5, Ranges: func(context.Context, int64, int64) (io.ReadCloser, error) {
		t.Error("single-range object opened a range")
		return nil, errors.New("unexpected")
	}})
	if err != nil || !result.Committed {
		t.Fatal(result, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "small")); string(data) != "small" {
		t.Fatalf("small object %q", data)
	}
}

// Every range waits until the one after it has finished, so the ranges are
// written strictly from last to first.
func TestLocalRangedWriteCompletesRangesOutOfOrder(t *testing.T) {
	const count = 8
	size := int64(count*testRangePart - 99)
	s, root, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: count})
	data := randomPayload(7, size)
	harness := newRangeHarness(t, data, 7)
	finished := make([]chan struct{}, count+1)
	for i := range finished {
		finished[i] = make(chan struct{})
	}
	close(finished[count])
	var order []int
	var mu sync.Mutex
	harness.hook = func(ctx context.Context, offset, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		index := int(offset / testRangePart)
		return &afterReader{ReadCloser: reader, ctx: ctx, wait: finished[index+1], done: func() {
			mu.Lock()
			order = append(order, index)
			mu.Unlock()
			close(finished[index])
		}}, nil
	}
	if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, data) {
		t.Fatal("content differs after out-of-order completion")
	}
	for i, index := range order {
		if index != count-1-i {
			t.Fatalf("completion order %v", order)
		}
	}
}

// afterReader delivers nothing until wait closes, and calls done at EOF.
type afterReader struct {
	io.ReadCloser
	ctx     context.Context
	wait    <-chan struct{}
	done    func()
	started bool
	ended   bool
}

func (r *afterReader) Read(p []byte) (int, error) {
	if !r.started {
		select {
		case <-r.wait:
			r.started = true
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !r.ended {
		r.ended = true
		r.done()
	}
	return n, err
}

// failAfter returns err once remaining bytes have been read.
type failAfter struct {
	io.ReadCloser
	remaining int64
	err       error
}

func (r *failAfter) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.err
	}
	p = p[:min(int64(len(p)), r.remaining)]
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

func TestLocalRangedWriteRetriesInterruptedRanges(t *testing.T) {
	size := int64(6*testRangePart + 10)
	s, root, registry := rangedLocal(t, LocalRangedOptions{PartConcurrency: 3})
	data := randomPayload(11, size)
	harness := newRangeHarness(t, data, 11)
	harness.hook = func(_ context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error) {
		switch {
		case offset == testRangePart && attempt == 1: // reset halfway
			return &failAfter{ReadCloser: reader, remaining: length / 2, err: errors.New("connection reset by peer")}, nil
		case offset == 3*testRangePart && attempt <= 2: // every byte, then a reset instead of EOF
			return &failAfter{ReadCloser: reader, remaining: length, err: io.ErrUnexpectedEOF}, nil
		}
		return reader, nil
	}
	if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, data) {
		t.Fatal("retried ranges corrupted content")
	}
	if harness.calls[testRangePart] != 2 || harness.calls[3*testRangePart] != 3 || harness.calls[0] != 1 {
		t.Fatalf("range requests %v", harness.calls)
	}
	if registry.pending() != 0 {
		t.Fatal("temporary record kept after success")
	}
}

func TestLocalRangedWriteFailuresPublishNothing(t *testing.T) {
	size := int64(6*testRangePart + 10)
	cases := map[string]struct {
		hook     func(ctx context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error)
		want     error
		attempts int // requests of range 2
	}{
		"retries exhausted": {func(_ context.Context, offset, length int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
			if offset == 2*testRangePart {
				return &failAfter{ReadCloser: reader, remaining: length / 3, err: errors.New("connection reset by peer")}, nil
			}
			return reader, nil
		}, nil, rangeReadAttempts},
		"short source": {func(_ context.Context, offset, length int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
			if offset == 2*testRangePart {
				return &failAfter{ReadCloser: reader, remaining: length - 1, err: io.EOF}, nil
			}
			return reader, nil
		}, ErrSizeMismatch, 1},
		"long source": {func(_ context.Context, offset, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
			if offset == 2*testRangePart {
				return io.NopCloser(io.MultiReader(reader, strings.NewReader("x"))), nil
			}
			return reader, nil
		}, ErrSizeMismatch, 1},
		"source changed on read": {func(_ context.Context, offset, length int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
			if offset == 2*testRangePart {
				return &failAfter{ReadCloser: reader, remaining: length / 2, err: ErrSourceChanged}, nil
			}
			return reader, nil
		}, ErrSourceChanged, 1},
		"source changed on open": {func(_ context.Context, offset, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
			if offset == 2*testRangePart {
				return nil, ErrSourceChanged
			}
			return reader, nil
		}, ErrSourceChanged, 1},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			s, root, registry := rangedLocal(t, LocalRangedOptions{PartConcurrency: 2})
			if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			harness := newRangeHarness(t, randomPayload(3, size), 3)
			harness.hook = test.hook
			result, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Overwrite: true, Ranges: harness.ranges})
			if err == nil || result.Committed || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("write: %+v %v", result, err)
			}
			if harness.calls[2*testRangePart] != test.attempts {
				t.Fatalf("range 2 requested %d times, want %d", harness.calls[2*testRangePart], test.attempts)
			}
			if _, active, _ := harness.state(); active != 0 {
				t.Fatal("range readers left open", active)
			}
			if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); string(got) != "previous" {
				t.Fatal("failed ranged write replaced the target")
			}
			assertOnlyFiles(t, root, "large.bin")
			if registry.pending() != 0 || registry.forgotten != 1 {
				t.Fatal("temporary record not cleared after failure")
			}
		})
	}
}

// A local source changed in place while ranges are read fails the object.
func TestLocalRangedWriteFromChangedLocalSource(t *testing.T) {
	sourceRoot := t.TempDir()
	size := int64(6*testRangePart + 1)
	data := randomPayload(5, size)
	name := filepath.Join(sourceRoot, "large.bin")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Unix(1700000000, 0)
	if err := os.Chtimes(name, past, past); err != nil {
		t.Fatal(err)
	}
	source, err := NewLocal(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	expected, err := source.Stat(context.Background(), "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, err := source.OpenRanges(context.Background(), "large.bin", OpenOptions{Expected: &expected})
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	s, root, registry := rangedLocal(t, LocalRangedOptions{PartConcurrency: 1})
	var once sync.Once
	ranges := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		if offset > 0 {
			once.Do(func() {
				file, err := os.OpenFile(name, os.O_WRONLY, 0)
				if err == nil {
					_, _ = file.WriteAt([]byte("CHANGED"), size/2)
					_ = file.Close()
				}
			})
		}
		return pinned.OpenRange(ctx, offset, length)
	}
	if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: ranges}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed local source: %v", err)
	}
	assertOnlyFiles(t, root)
	if registry.pending() != 0 {
		t.Fatal("temporary record kept")
	}
}

func TestLocalRangedWriteCancellationStopsEveryReader(t *testing.T) {
	const base = 4
	size := int64(12 * testRangePart)
	s, root, registry := rangedLocal(t, LocalRangedOptions{PartConcurrency: base})
	harness := newRangeHarness(t, randomPayload(13, size), 13)
	var blocked sync.WaitGroup
	blocked.Add(base)
	harness.hook = func(ctx context.Context, _, length int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		// Half of each range arrives, then the body stalls until closed.
		return &stallReader{ReadCloser: reader, remaining: length / 2, stalled: blocked.Done, release: make(chan struct{})}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(ctx, "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges})
		done <- err
	}()
	blocked.Wait()
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute): // hang guard only
		t.Fatal("cancellation did not stop the range readers")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
	if open, active, _ := harness.state(); open != base || active != 0 {
		t.Fatalf("opened %d ranges, %d still open", open, active)
	}
	assertOnlyFiles(t, root)
	if registry.pending() != 0 {
		t.Fatal("temporary record kept after cancellation")
	}
	if used, waiting := memoryState(s.ranged.Memory); used != 0 || waiting != 0 {
		t.Fatalf("cancellation kept memory: used=%d waiting=%d", used, waiting)
	}
}

// stallReader delivers remaining bytes, then blocks until it is closed.
type stallReader struct {
	io.ReadCloser
	remaining int64
	stalled   func()
	once      sync.Once
	release   chan struct{}
	closeOnce sync.Once
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		r.once.Do(r.stalled)
		<-r.release
		return 0, errors.New("use of closed network connection")
	}
	p = p[:min(int64(len(p)), r.remaining)]
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *stallReader) Close() error {
	r.closeOnce.Do(func() { close(r.release) })
	return r.ReadCloser.Close()
}

// overlapRanges holds the first n ranges open until n are open together, so
// the peak does not depend on scheduling.
func overlapRanges(n int) func(ctx context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error) {
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	return func(ctx context.Context, _, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		held := arrived <= n
		mu.Unlock()
		if held {
			select {
			case <-all:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return reader, nil
	}
}

func TestLocalRangedWriteBorrowsIdleSlotsAndReturnsThem(t *testing.T) {
	cases := []struct {
		name            string
		idle, base      int
		parts, overlap  int
		minPeak, maxMax int
	}{
		{"busy node keeps base slots", 0, 2, 10, 2, 2, 2},
		{"idle node lends extra slots", 100, 2, 10, 6, 6, maxPartSlots},
		{"borrowing stops at sixteen", 100, 4, 40, maxPartSlots, maxPartSlots, maxPartSlots},
		{"lending follows idle capacity", 1, 2, 10, 4, 4, 4},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			lender := NewPartLender()
			var borrowed atomic.Bool
			lender.SetIdle(func() int { return test.idle })
			s, root, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: test.base, PartLender: lender})
			size := int64(test.parts) * testRangePart
			data := randomPayload(17, size)
			harness := newRangeHarness(t, data, 17)
			overlap := overlapRanges(test.overlap)
			harness.hook = func(ctx context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error) {
				if lender.Lent() > 0 {
					borrowed.Store(true)
				}
				return overlap(ctx, offset, length, attempt, reader)
			}
			if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, data) {
				t.Fatal("content differs")
			}
			_, _, peak := harness.state()
			if peak < test.minPeak || peak > test.maxMax {
				t.Fatalf("peak concurrent ranges %d, want %d..%d", peak, test.minPeak, test.maxMax)
			}
			if test.idle == 0 && borrowed.Load() {
				t.Fatal("busy node lent slots")
			}
			if lender.Lent() != 0 {
				t.Fatal("borrowed slots not returned", lender.Lent())
			}
		})
	}
}

// Borrowed slots return when an object fails as well.
func TestLocalRangedWriteReturnsBorrowedSlotsOnFailure(t *testing.T) {
	lender := NewPartLender()
	lender.SetIdle(func() int { return 100 })
	s, _, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 1, PartLender: lender})
	size := int64(10 * testRangePart)
	harness := newRangeHarness(t, randomPayload(19, size), 19)
	overlap := overlapRanges(4)
	harness.hook = func(ctx context.Context, offset, length int64, attempt int, reader io.ReadCloser) (io.ReadCloser, error) {
		reader, err := overlap(ctx, offset, length, attempt, reader)
		if err == nil && offset == 2*testRangePart {
			return &failAfter{ReadCloser: reader, remaining: 1, err: ErrSourceChanged}, nil
		}
		return reader, err
	}
	if _, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); !errors.Is(err, ErrSourceChanged) {
		t.Fatal(err)
	}
	if _, active, peak := harness.state(); active != 0 || peak < 4 || lender.Lent() != 0 {
		t.Fatalf("active=%d peak=%d lent=%d", active, peak, lender.Lent())
	}
}

func TestLocalRangedWriteKeepsSkipSemantics(t *testing.T) {
	s, root, registry := rangedLocal(t, LocalRangedOptions{})
	if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(3 * testRangePart)
	harness := newRangeHarness(t, randomPayload(23, size), 23)
	result, err := s.Write(context.Background(), "large.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges})
	if err != nil || !result.Skipped || len(harness.calls) != 0 {
		t.Fatalf("SKIP over existing target: %+v %v calls=%v", result, err, harness.calls)
	}
	// A target created while ranges download wins over the new object.
	harness.hook = func(_ context.Context, offset, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		if offset == 0 {
			if err := os.WriteFile(filepath.Join(root, "race.bin"), []byte("concurrent"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return reader, nil
	}
	result, err = s.Write(context.Background(), "race.bin", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges})
	if err != nil || !result.Skipped || result.Committed {
		t.Fatalf("concurrently created target: %+v %v", result, err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "race.bin")); string(got) != "concurrent" {
		t.Fatal("ranged SKIP replaced a concurrent target")
	}
	assertOnlyFiles(t, root, "large.bin", "race.bin")
	if registry.pending() != 0 {
		t.Fatal("temporary record kept")
	}
	if _, err := s.Write(context.Background(), "../escape", unusedInput{t}, WriteOptions{Size: size, Ranges: harness.ranges}); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("path escape accepted", err)
	}
}
