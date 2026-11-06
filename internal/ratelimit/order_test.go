package ratelimit

import (
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/MoonWX/oss-transfer/internal/readorder"
)

// readRanked reads length bytes through a limiter reader whose context ranks
// it in queue, adding every byte to passed.
func readRanked(f *fixture, queue *readorder.Queue, rank int64, input io.Reader, length int64, passed *atomic.Int64) error {
	r, err := f.limiter.Wrap(readorder.With(bg, queue, rank), 1, 1, io.NopCloser(input), length)
	if err != nil {
		return err
	}
	defer r.Close()
	buffer := make([]byte, 64<<10)
	for read := int64(0); read < length; {
		n, err := r.Read(buffer)
		read += int64(n)
		passed.Add(int64(n))
		if err != nil {
			return err
		}
	}
	return nil
}

// Ranges of one ordered write share a parent bucket lowest rank first, so they
// finish one after another instead of together at the end, as an even share
// would have them: the k-th range finishes before about k+2 ranges' worth of
// bytes have passed the limiter. The fake clock makes the grants exact.
func TestOrderedRangesTakeBandwidthLowestRankFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		apply(t, f.limiter, 0, 4<<20)
		const ranges = 8
		const length = 512 << 10
		queue := readorder.NewQueue()
		var passed atomic.Int64
		finished := make(chan int64, ranges)
		for rank := range int64(ranges) {
			go func() {
				if err := readRanked(f, queue, rank, io.LimitReader(zeroes{}, length), length, &passed); err != nil {
					t.Error(err)
				}
				finished <- passed.Load()
			}()
		}
		// Before the low ranks queue up, one higher rank may take a read.
		const slack = 4 * 64 << 10
		for k := range int64(ranges) {
			if at, limit := <-finished, (k+2)*length+slack; at > limit {
				t.Fatalf("range %d of %d finished after %d bytes, want at most %d", k+1, ranges, at, limit)
			}
		}
	})
}

// gatedSource yields its first read, then blocks until opened.
type gatedSource struct {
	first bool
	open  chan struct{}
}

func (s *gatedSource) Read(p []byte) (int, error) {
	if s.first {
		<-s.open
	}
	s.first = true
	return len(p), nil
}

// A range waits for tokens in its turn but reads its source after passing the
// turn on: the lowest range blocked on a silent connection does not stop the
// ranges after it from using the rate.
func TestRangeBlockedOnItsSourceHoldsNoTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		apply(t, f.limiter, 0, 4<<20)
		const length = 256 << 10
		queue := readorder.NewQueue()
		var passed atomic.Int64
		stalled := &gatedSource{open: make(chan struct{})}
		lowest := make(chan error, 1)
		go func() { lowest <- readRanked(f, queue, 0, io.LimitReader(stalled, length), length, &passed) }()
		later := make(chan error, 3)
		for rank := int64(1); rank <= 3; rank++ {
			go func() { later <- readRanked(f, queue, rank, io.LimitReader(zeroes{}, length), length, &passed) }()
		}
		// A turn held across the stalled read would block these forever, and
		// the fake clock would report the bubble deadlocked.
		for range 3 {
			if err := <-later; err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err := <-lowest:
			t.Fatalf("stalled range finished early: %v", err)
		default:
		}
		close(stalled.open)
		if err := <-lowest; err != nil {
			t.Fatal(err)
		}
	})
}
