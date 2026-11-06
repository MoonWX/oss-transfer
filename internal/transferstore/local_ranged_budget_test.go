package transferstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRangeBudgetWindowIsAboutOneSecondOfShare(t *testing.T) {
	const chunk = 8 << 20
	for _, tc := range []struct {
		name   string
		budget RangeBudget
		want   int64
	}{
		{"unlimited", RangeBudget{}, 0},
		{"bandwidth", RangeBudget{BytesPerSecond: 992_000_000 / 8}, 992_000_000 / 8},
		{"get rate", RangeBudget{RequestsPerSecond: 3}, 3 * chunk},
		{"smaller of both", RangeBudget{BytesPerSecond: 1 << 30, RequestsPerSecond: 2.5}, 5 * chunk / 2},
		{"huge get rate", RangeBudget{RequestsPerSecond: 1e9}, 1e9 * chunk},
	} {
		if got := tc.budget.Window(chunk); got != tc.want {
			t.Errorf("%s: window %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestLocalRangedWriteWithinNeedsTwoChunksPerSecond(t *testing.T) {
	s, _, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 4})
	size := int64(20 * testRangePart)
	for _, tc := range []struct {
		budget RangeBudget
		ranged bool
		slots  int
	}{
		{RangeBudget{BytesPerSecond: 2*testRangePart - 1}, false, 1},
		{RangeBudget{BytesPerSecond: 2 * testRangePart}, true, 2},
		{RangeBudget{BytesPerSecond: 1 << 40, RequestsPerSecond: 1.5}, false, 1},
		{RangeBudget{BytesPerSecond: 1 << 40}, true, maxPartSlots},
		{RangeBudget{}, true, maxPartSlots}, // a PUT-only limit: fetches never wait
	} {
		if got := s.RangedWriteWithin(size, tc.budget); got != tc.ranged {
			t.Errorf("%+v: ranged %v, want %v", tc.budget, got, tc.ranged)
		}
		if got := s.windowSlots(&tc.budget); got != tc.slots {
			t.Errorf("%+v: %d slots, want %d", tc.budget, got, tc.slots)
		}
	}
	if s.RangedWriteWithin(testRangePart, RangeBudget{BytesPerSecond: 1 << 40}) {
		t.Error("a one-chunk object ranged")
	}
}

// A throttled object never holds more chunks than about one second of its
// share, even with idle node slots to borrow and a writer that stalls so every
// slot it may take fills up.
func TestThrottledLocalRangedWritePinsAtMostItsWindow(t *testing.T) {
	const window = 3
	budget, _ := NewUploadMemoryBudget(64 << 20)
	lender := NewPartLender()
	lender.SetIdle(func() int { return 100 })
	s, _, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 4, PartLender: lender, Memory: budget})
	share := RangeBudget{BytesPerSecond: window*testRangePart + testRangePart/2}
	slots := s.windowSlots(&share)
	if slots != window {
		t.Fatalf("window %d slots, want %d", slots, window)
	}
	size := int64(12 * testRangePart)
	data := randomPayload(43, size)
	harness := newRangeHarness(t, data, 43)
	var fetched sync.WaitGroup
	fetched.Add(window)
	var count atomic.Int64
	harness.hook = func(ctx context.Context, _, _ int64, _ int, reader io.ReadCloser) (io.ReadCloser, error) {
		if count.Add(1) <= window {
			return &gatedRange{ReadCloser: reader, ctx: ctx, gate: closedGate, eof: fetched.Done}, nil
		}
		return reader, nil
	}
	writer := &recordingWriter{entered: make(chan struct{}), block: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.writeRanges(context.Background(), writer, harness.ranges, size, slots) }()
	<-writer.entered
	fetched.Wait()
	for range 3 {
		if open, _, _ := harness.state(); open != window {
			t.Fatalf("fetched %d chunks, window %d", open, window)
		}
		if used, _ := memoryState(budget); used != window*testRangePart {
			t.Fatalf("throttled object pins %d bytes, cap %d", used, window*testRangePart)
		}
		if lender.Lent() != 0 {
			t.Fatal("throttled object borrowed past its window", lender.Lent())
		}
		runtime.Gosched()
	}
	close(writer.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.data.Bytes(), data) {
		t.Fatal("content differs")
	}
	if _, _, peak := harness.state(); peak > window {
		t.Fatalf("peak %d concurrent chunks, window %d", peak, window)
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 || lender.Lent() != 0 {
		t.Fatalf("used=%d waiting=%d lent=%d", used, waiting, lender.Lent())
	}
}

// Under two chunks a second, Write streams its input and opens no range.
func TestThrottledLocalWriteWithTinyShareStreams(t *testing.T) {
	s, root, _ := rangedLocal(t, LocalRangedOptions{PartConcurrency: 4})
	size := int64(6 * testRangePart)
	data := randomPayload(47, size)
	harness := newRangeHarness(t, data, 47)
	share := RangeBudget{BytesPerSecond: testRangePart}
	if _, err := s.Write(context.Background(), "small-share.bin", bytes.NewReader(data), WriteOptions{Size: size, Ranges: harness.ranges, RangeBudget: &share}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "small-share.bin")); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if open, _, _ := harness.state(); open != 0 {
		t.Fatal("tiny share opened ranges", open)
	}
}
