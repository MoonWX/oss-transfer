package transferstore_test

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// throttledShare is a 1000 Mbps task share, all of it on this node.
const throttledShare = 125_000_000

// pacedSink models a FUSE target: writes drain at rate bytes per second and
// stall for stall after every stallEvery bytes, as when the filesystem
// flushes its write-back buffer. It sleeps against a schedule, so timer
// overshoot does not slow it below its rate, and idle time earns no credit.
type pacedSink struct {
	rate, stallEvery int64
	stall            time.Duration
	written          int64
	due              time.Time
}

func (s *pacedSink) Write(p []byte) (int, error) {
	cost := time.Duration(int64(len(p)) * int64(time.Second) / s.rate)
	cost += time.Duration((s.written+int64(len(p)))/s.stallEvery-s.written/s.stallEvery) * s.stall
	s.written += int64(len(p))
	if floor := time.Now().Add(-2 * time.Millisecond); s.due.Before(floor) {
		s.due = floor
	}
	s.due = s.due.Add(cost)
	if wait := time.Until(s.due); wait >= time.Millisecond {
		time.Sleep(wait)
	}
	return len(p), nil
}

// pacedRange models one source connection of remaining bytes: they arrive at
// rate per second into a receive buffer of at most window bytes, and a read
// returns what has arrived, waiting only while too little has. Zero rate is
// unpaced. Content is irrelevant here, so the bytes are never filled.
type pacedRange struct {
	remaining, rate, window int64
	buffered                int64
	last                    time.Time
	fraction                float64
}

func (r *pacedRange) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	if r.rate > 0 {
		need := min(n, 64<<10)
		for {
			now := time.Now()
			arrived := now.Sub(r.last).Seconds()*float64(r.rate) + r.fraction
			r.last = now
			whole := int64(arrived)
			r.fraction = arrived - float64(whole)
			r.buffered = min(r.window, r.remaining, r.buffered+whole)
			if r.buffered >= need {
				break
			}
			time.Sleep(time.Duration(float64(need-r.buffered) / float64(r.rate) * float64(time.Second)))
		}
		n = min(n, r.buffered)
		r.buffered -= n
	}
	r.remaining -= n
	return int(n), nil
}

// newThrottledTarget is a Local that ranges with chunk-sized ranges (0 is the
// default 8 MiB), as one object on an otherwise idle node: its throttled
// window can borrow every slot it may hold.
func newThrottledTarget(tb testing.TB, chunk int64) (*transferstore.Local, *state.Store) {
	tb.Helper()
	target, err := transferstore.NewLocal(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { target.Close() })
	memory, err := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	if err != nil {
		tb.Fatal(err)
	}
	lender := transferstore.NewPartLender()
	lender.SetIdle(func() int { return 100 })
	if err := target.SetRangedWrites(transferstore.LocalRangedOptions{MinSize: 1, ChunkSize: chunk, PartLender: lender, Memory: memory}); err != nil {
		tb.Fatal(err)
	}
	db, err := state.Open(filepath.Join(tb.TempDir(), "agent.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return target, db
}

// throttledDownload copies size bytes into sink under a task share through
// the real limiter, from connections paced to connection bytes per second,
// and returns the time it took. "stream" copies one source stream as
// Local.Write streams it (an *os.File target makes io.CopyBuffer fall back to
// 32 KiB reads); "ranged" runs the ordered ranged writer with the window
// Write would use for the share. Each run uses a new limiter generation, so
// no run inherits earned credit.
func throttledDownload(tb testing.TB, target *transferstore.Local, db *state.Store, generation uint64, mode string, size, share, connection int64, sink *pacedSink) time.Duration {
	tb.Helper()
	ctx := context.Background()
	limits, err := ratelimit.New(db, ratelimit.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	defer limits.Close()
	if err := limits.ApplyParent(ctx, ratelimit.ParentLimit{ParentID: 1, Generation: generation, Revision: 1, BandwidthBytesPerSecond: share}); err != nil {
		tb.Fatal(err)
	}
	open := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		source := &pacedRange{remaining: length, rate: connection, window: max(64<<10, connection/16), last: time.Now()}
		return limits.Wrap(ctx, 1, generation, io.NopCloser(source), length)
	}
	budget := &transferstore.RangeBudget{BytesPerSecond: share}
	if mode == "ranged" && !target.RangedWriteWithin(size, *budget) {
		tb.Fatal("the share does not range")
	}
	started := time.Now()
	switch mode {
	case "stream":
		var input io.ReadCloser
		if input, err = open(ctx, 0, size); err == nil {
			var written int64
			written, err = io.Copy(struct{ io.Writer }{sink}, io.LimitReader(input, size))
			if err == nil && written != size {
				err = fmt.Errorf("copied %d of %d bytes", written, size)
			}
			_ = input.Close()
		}
	case "ranged":
		err = target.WriteRangesForTest(ctx, sink, open, size, budget)
	}
	elapsed := time.Since(started)
	if err != nil {
		tb.Fatal(err)
	}
	if sink.written != size {
		tb.Fatalf("sink received %d of %d bytes", sink.written, size)
	}
	return elapsed
}

// Under a task share, an object downloaded in ranges reaches the share, even
// though the target stalls and, in the paced case, one connection alone is
// slower than the share. Chunks take tokens lowest offset first, so they
// complete one after another and the writer appends each as it lands. With
// an even share the whole window finished together, and at the end of the
// object the network idled while the writer drained it: 90-94% here. This is
// the benchmark below scaled down eightfold, on the fake clock; the limiter's
// own accounting bounds it from above.
func TestThrottledRangedDownloadReachesItsShare(t *testing.T) {
	for _, connection := range []int64{0, 8 << 20} {
		t.Run(fmt.Sprintf("connection-%d", connection), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const size = 32 << 20
				const share = throttledShare / 8
				target, db := newThrottledTarget(t, 1<<20)
				sink := &pacedSink{rate: 50 << 20, stallEvery: 2 << 20, stall: 20 * time.Millisecond}
				elapsed := throttledDownload(t, target, db, 1, "ranged", size, share, connection, sink)
				if allowed := share * elapsed.Seconds(); size > allowed+1 {
					t.Fatalf("ranged download exceeded its share: %d bytes in %v, allowed %.0f", size, elapsed, allowed)
				}
				got := size / elapsed.Seconds() / share
				t.Logf("%.1f%% of the share in %v", got*100, elapsed)
				if got < 0.95 {
					t.Fatalf("ranged download reached %.1f%% of its share in %v", got*100, elapsed)
				}
			})
		})
	}
}

// BenchmarkThrottledLocalDownload measures one 256 MiB object downloaded under
// a 1000 Mbps share into a sink that drains 400 MiB/s and stalls for 20 ms
// after every 16 MiB, through the real limiter, as one stream and in ranges
// (14 chunks of 8 MiB), from an unpaced source ("memory") and with every
// connection paced to 64 MiB/s ("conn64MiBps"), where one stream cannot reach
// the share. %share is the achieved fraction of the share. Run on Linux too:
// Go rounds its sub-millisecond timer waits up to a millisecond there.
//
//	go test ./internal/transferstore -run '^$' -bench BenchmarkThrottledLocalDownload -benchtime 3x
func BenchmarkThrottledLocalDownload(b *testing.B) {
	const size = 256 << 20
	for _, connection := range []int64{0, 64 << 20} {
		network := "memory"
		if connection > 0 {
			network = fmt.Sprintf("conn%dMiBps", connection>>20)
		}
		for _, mode := range []string{"stream", "ranged"} {
			b.Run(network+"/"+mode, func(b *testing.B) {
				target, db := newThrottledTarget(b, 0)
				var elapsed time.Duration
				b.SetBytes(size)
				for run := range b.N {
					sink := &pacedSink{rate: 400 << 20, stallEvery: 16 << 20, stall: 20 * time.Millisecond}
					elapsed += throttledDownload(b, target, db, uint64(run+1), mode, size, throttledShare, connection, sink)
				}
				b.ReportMetric(float64(size)*float64(b.N)/elapsed.Seconds()/throttledShare*100, "%share")
			})
		}
	}
}
