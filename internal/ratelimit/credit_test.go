package ratelimit

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestHighRateReadersShareEarnedCredit(t *testing.T) {
	for _, scope := range []string{"node", "parent"} {
		t.Run(scope, func(t *testing.T) { testHighRateReaders(t, scope) })
	}
}

func testHighRateReaders(t *testing.T, scope string) {
	t.Helper()
	f := newFixture(t)
	const rate = 125000000 // 1000 Mbps; earned window exceeds one 64 KiB chunk.
	parents := 4
	nodeRate, parentRate := int64(rate), int64(0)
	if scope == "parent" {
		parents = 1
		nodeRate = 0
		parentRate = rate
	}
	apply(t, f.limiter, nodeRate, parentRate)
	for id := int64(2); id <= int64(parents); id++ {
		if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: id, Generation: 1, Revision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	const readers = 16
	const perReader = 8 << 20
	data := make([]byte, perReader)
	errs := make(chan error, readers)
	var workers sync.WaitGroup
	started := time.Now()
	for index := range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			r, err := f.limiter.Wrap(bg, int64(index%parents+1), 1, io.NopCloser(bytes.NewReader(data)), perReader)
			if err != nil {
				errs <- err
				return
			}
			defer r.Close()
			n, err := io.Copy(io.Discard, r)
			if err == nil && n != perReader {
				err = fmt.Errorf("short read: %d", n)
			}
			errs <- err
		}()
	}
	workers.Wait()
	elapsed := time.Since(started)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	minimum := time.Duration(float64(readers*perReader) / rate * float64(time.Second))
	// Credit starts empty and is shared across every reader and parent. This
	// checks the actual source-byte path, not a reported bandwidth counter.
	if elapsed < minimum-2*time.Millisecond {
		t.Fatalf("shared budget exceeded its rate: %d bytes in %v; minimum %v", readers*perReader, elapsed, minimum)
	}
}

func TestCreditWindowEarnsBudgetWithoutInitialOrUnboundedIdleBurst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate     int64
		capacity float64
	}{
		{"low-rate-keeps-one-read", 500000, 65536},
		{"1000-Mbps-100-milliseconds", 125000000, 12500000},
		{"extreme-rate-capped-at-16-MiB", 1 << 50, 16 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1000, 0)
			b := bucket{rate: tc.rate, last: start}
			if got := b.capacity(64 << 10); got != tc.capacity {
				t.Fatalf("maximum earned credit: got %v, want %v", got, tc.capacity)
			}
			// The first read can follow configuration by hours. None of that
			// time counts as earned payload credit.
			firstRead := start.Add(time.Hour)
			b.refill(firstRead, 64<<10)
			if b.tokens != 0 {
				t.Fatalf("free initial credit: %v", b.tokens)
			}
			b.refill(firstRead.Add(time.Microsecond), 64<<10)
			want := math.Min(tc.capacity, float64(tc.rate)/1000000)
			if math.Abs(b.tokens-want) > 0.000001 {
				t.Fatalf("credit not backed by elapsed time: got %v, want %v", b.tokens, want)
			}
			b.refill(firstRead.Add(time.Hour), 64<<10)
			if b.tokens != tc.capacity {
				t.Fatalf("idle credit exceeded bound: %v", b.tokens)
			}
			b.take(int(tc.capacity))
			b.refill(firstRead.Add(time.Hour), 64<<10)
			if b.tokens != 0 {
				t.Fatalf("same instant manufactured credit: %v", b.tokens)
			}
		})
	}
}

func TestPolicyUpdatesDiscardEarnedWindowCredit(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 250000000, 250000000)
	l := f.limiter
	l.mu.Lock()
	l.nodeBudget.tokens = 250000
	l.nodeBudget.started = true
	l.parents[1].budget.tokens = 250000
	l.parents[1].budget.started = true
	l.mu.Unlock()
	if err := l.ApplyNode(bg, NodeLimit{Revision: 2, BandwidthBytesPerSecond: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, BandwidthBytesPerSecond: 1000}); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodeBudget.tokens != 0 || l.nodeBudget.started || l.parents[1].budget.tokens != 0 || l.parents[1].budget.started {
		t.Fatal("old window credit survived a lower policy revision")
	}
}

func TestRefundCannotExceedEarnedWindowBound(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 250000000, 250000000)
	// Multiple in-flight short reads may return their reservations together.
	// Even matching revisions must not enlarge the node or parent burst bound.
	ticket := reservation{size: 64 << 10, nodeRevision: 1, parentRevision: 1, generation: 1, parentID: 1}
	for range 2 * maximumCreditBytes / ticket.size {
		f.limiter.refund(ticket, ticket.size)
	}
	f.limiter.mu.Lock()
	defer f.limiter.mu.Unlock()
	if f.limiter.nodeBudget.tokens != maximumCreditBytes || f.limiter.parents[1].budget.tokens != maximumCreditBytes {
		t.Fatal("refunded credit exceeded the 16 MiB burst bound")
	}
}

// zeroes yields any number of bytes; limiter tests need counts, not content.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }

// A stream alternates source reads with writes, here to a target that writes
// 400 MiB/s but stalls for 20 ms after every 8 MiB. It earns credit while it
// writes and keeps up to 100 ms of it, so after each stall it reads ahead and
// still reaches its 1000 Mbps share (a 1 ms bucket held it to about 80%). The
// fake clock makes this exact, and the bucket's own accounting bounds it from
// above: from an empty bucket, no stream reads more than its rate times the
// elapsed time.
func TestStreamWithWriteStallsKeepsItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		const rate = 125000000
		const size = 128 << 20
		apply(t, f.limiter, 0, rate)
		r, err := f.limiter.Wrap(bg, 1, 1, io.NopCloser(io.LimitReader(zeroes{}, size)), size)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		// io.Copy into an *os.File, as Local.Write streams, reads 32 KiB at a time.
		buffer := make([]byte, 32<<10)
		start := time.Now()
		var read int64
		var elapsed time.Duration
		for read < size {
			n, err := r.Read(buffer)
			if err != nil {
				t.Fatal(err)
			}
			read += int64(n)
			elapsed = time.Since(start)
			write := time.Duration(int64(n) * int64(time.Second) / (400 << 20))
			if read/(8<<20) != (read-int64(n))/(8<<20) {
				write += 20 * time.Millisecond
			}
			time.Sleep(write)
		}
		if allowed := rate * elapsed.Seconds(); float64(size) > allowed+1 {
			t.Fatalf("stream exceeded its share: %d bytes in %v, allowed %.0f", size, elapsed, allowed)
		}
		if share := float64(size) / elapsed.Seconds() / rate; share < 0.95 {
			t.Fatalf("stream reached %.1f%% of its share in %v", share*100, elapsed)
		}
	})
}
