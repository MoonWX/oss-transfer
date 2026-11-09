package comparison

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Content read limits. Package variables so tests can shorten them.
var (
	// contentRangeSize is the length of each sequential range read. Objects
	// of at most one range are read with a single GET.
	contentRangeSize int64 = 8 << 20
	// readIdleTimeout aborts a read attempt when opening it or one read call
	// makes no progress for this long. The attempt is then retried.
	readIdleTimeout = 60 * time.Second
	// rangeRetryDelays are the waits before each further attempt at a range;
	// a retry continues at the first byte not yet hashed.
	rangeRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	// streamRetryDelays are the waits before each further attempt at reading
	// a whole object with Open; a retry hashes it from the start again.
	streamRetryDelays = []time.Duration{time.Second, 2 * time.Second}
)

const contentReadBuffer = 64 << 10

// hashBoth computes the SHA-256 of both objects concurrently. When one side
// fails the other stops early; its resulting cancellation is not reported.
func (c *comparer) hashBoth(ctx context.Context, source, target transferstore.Object) (sourceSum, targetSum string, sourceErr, targetErr error) {
	hashCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		targetSum, targetErr = c.hash(hashCtx, c.target, target)
		if targetErr != nil {
			cancel()
		}
	}()
	sourceSum, sourceErr = c.hash(hashCtx, c.source, source)
	if sourceErr != nil {
		cancel()
	}
	<-done
	if ctx.Err() == nil && sourceErr != nil && targetErr != nil {
		if errors.Is(targetErr, context.Canceled) {
			targetErr = nil
		} else if errors.Is(sourceErr, context.Canceled) {
			sourceErr = nil
		}
	}
	return sourceSum, targetSum, sourceErr, targetErr
}

// contentHash accumulates one object's SHA-256 and reports hashed bytes.
type contentHash struct {
	sum     hash.Hash
	buffer  []byte
	onBytes func(int64)
}

// hash reads object, pinned to the expected version, into a SHA-256. Objects
// larger than one range are read as sequential ranges when the store offers
// them; otherwise, or when the store cannot serve this object in ranges, the
// object is streamed with Open.
func (c *comparer) hash(ctx context.Context, store transferstore.Store, object transferstore.Object) (string, error) {
	h := &contentHash{sum: sha256.New(), buffer: make([]byte, contentReadBuffer), onBytes: c.onBytes}
	if opener, ok := store.(transferstore.RangeOpener); ok && object.Size > contentRangeSize {
		unsupported, err := c.hashRanges(ctx, opener, object, h)
		if err != nil {
			return "", err
		}
		if !unsupported {
			return hex.EncodeToString(h.sum.Sum(nil)), nil
		}
	}
	if err := c.hashStream(ctx, store, object, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.sum.Sum(nil)), nil
}

// hashRanges reads object as sequential ranges of contentRangeSize. A failed
// range is retried from its first unread byte, so every byte is hashed once.
// unsupported reports that the store cannot serve this object in ranges; no
// byte has been hashed then.
func (c *comparer) hashRanges(ctx context.Context, opener transferstore.RangeOpener, object transferstore.Object, h *contentHash) (unsupported bool, err error) {
	var ranges transferstore.RangeSource
	err = retryContentRead(ctx, rangeRetryDelays, func(attemptCtx context.Context, watch *readWatchdog) error {
		watch.arm()
		opened, _, err := opener.OpenRanges(attemptCtx, object.Key, transferstore.OpenOptions{Expected: &object})
		watch.disarm()
		if err == nil {
			ranges = opened
		}
		return err
	})
	if errors.Is(err, transferstore.ErrUnsupported) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer ranges.Close()
	for offset := int64(0); offset < object.Size; {
		end := min(offset+contentRangeSize, object.Size)
		err := retryContentRead(ctx, rangeRetryDelays, func(attemptCtx context.Context, watch *readWatchdog) error {
			if offset >= end {
				// An earlier attempt hashed every byte and failed only while
				// waiting for the end of the body.
				return nil
			}
			watch.arm()
			body, err := ranges.OpenRange(attemptCtx, offset, end-offset)
			watch.disarm()
			if err != nil {
				return err
			}
			read, err := c.consume(attemptCtx, watch, body, end-offset, h)
			offset += read
			return err
		})
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// hashStream reads the whole object with one GET. A failed attempt starts the
// hash again with a fresh conditional GET.
func (c *comparer) hashStream(ctx context.Context, store transferstore.Store, object transferstore.Object, h *contentHash) error {
	return retryContentRead(ctx, streamRetryDelays, func(attemptCtx context.Context, watch *readWatchdog) error {
		h.sum.Reset()
		watch.arm()
		body, _, err := store.Open(attemptCtx, object.Key, transferstore.OpenOptions{Expected: &object})
		watch.disarm()
		if err != nil {
			return err
		}
		_, err = c.consume(attemptCtx, watch, body, object.Size, h)
		return err
	})
}

// consume hashes exactly length bytes of body and closes it. The watchdog
// observes the storage reader itself, below the node's bandwidth limiter, so
// waiting for bandwidth is not mistaken for a stalled read.
func (c *comparer) consume(ctx context.Context, watch *readWatchdog, body io.ReadCloser, length int64, h *contentHash) (int64, error) {
	var reader io.ReadCloser = &watchedReader{ReadCloser: body, watch: watch}
	if c.wrap != nil {
		wrapped, err := c.wrap(ctx, reader, length)
		if err != nil {
			_ = reader.Close()
			return 0, err
		}
		reader = wrapped
	}
	read, err := h.copy(ctx, reader, length)
	_ = reader.Close()
	return read, err
}

// copy hashes the reader until EOF and requires exactly length bytes. A read
// returning neither data nor an error ends when ctx does, so a reader that
// ignores cancellation cannot spin past the idle watchdog.
func (h *contentHash) copy(ctx context.Context, reader io.Reader, length int64) (int64, error) {
	var read int64
	for {
		n, err := reader.Read(h.buffer)
		if n == 0 && err == nil {
			err = ctx.Err()
		}
		if n > 0 {
			if int64(n) > length-read {
				return read, fmt.Errorf("%w: storage returned more bytes than the object size", transferstore.ErrSizeMismatch)
			}
			h.sum.Write(h.buffer[:n])
			read += int64(n)
			if h.onBytes != nil {
				h.onBytes(int64(n))
			}
		}
		if errors.Is(err, io.EOF) {
			if read < length {
				return read, fmt.Errorf("%w: storage returned fewer bytes than the object size", transferstore.ErrSizeMismatch)
			}
			return read, nil
		}
		if err != nil {
			return read, err
		}
	}
}

// retryContentRead runs attempt until it succeeds, fails finally, ctx ends, or
// the retries in delays are used up. Each attempt has its own context, which
// its read watchdog cancels when the attempt stalls.
func retryContentRead(ctx context.Context, delays []time.Duration, attempt func(context.Context, *readWatchdog) error) error {
	for try := 0; ; try++ {
		err := watchedAttempt(ctx, attempt)
		if err == nil || ctx.Err() != nil || finalReadError(err) || try >= len(delays) {
			return err
		}
		if !sleepComparison(ctx, delays[try]) {
			return ctx.Err()
		}
	}
}

func watchedAttempt(ctx context.Context, attempt func(context.Context, *readWatchdog) error) error {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	watch := &readWatchdog{timeout: readIdleTimeout, cancel: cancel}
	err := attempt(attemptCtx, watch)
	watch.stop()
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(attemptCtx), errComparisonReadIdle) {
		err = &comparisonTimeoutError{operation: "GET", limit: watch.timeout, idle: true}
	}
	return err
}

// readWatchdog cancels its attempt when a storage call it is armed for makes
// no progress within timeout. It is armed while opening a body and during each
// read call, and disarmed when data or an error arrives.
type readWatchdog struct {
	mu      sync.Mutex
	timeout time.Duration
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	// generation numbers each arming. A timer that expires as it is being
	// disarmed runs anyway; it must not cancel a later arming.
	generation uint64
	armed      bool
	fired      bool
	// running counts timer callbacks that are scheduled or running, so stop
	// can wait until none outlives the attempt.
	running sync.WaitGroup
}

func (w *readWatchdog) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.armed || w.fired {
		return
	}
	w.armed = true
	w.generation++
	generation := w.generation
	w.running.Add(1)
	w.timer = time.AfterFunc(w.timeout, func() { w.fire(generation) })
}

func (w *readWatchdog) disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed {
		return
	}
	w.armed = false
	if w.timer.Stop() {
		w.running.Done()
	}
	// Otherwise the callback has started; it finds itself stale and returns.
}

func (w *readWatchdog) fire(generation uint64) {
	defer w.running.Done()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed || w.generation != generation {
		return
	}
	w.armed, w.fired = false, true
	w.cancel(errComparisonReadIdle)
}

func (w *readWatchdog) stop() {
	w.disarm()
	w.running.Wait()
}

// watchedReader arms the watchdog for each read call of the storage body. A
// read returning neither data nor an error leaves it armed.
type watchedReader struct {
	io.ReadCloser
	watch *readWatchdog
}

func (r *watchedReader) Read(p []byte) (int, error) {
	r.watch.arm()
	n, err := r.ReadCloser.Read(p)
	if n > 0 || err != nil {
		r.watch.disarm()
	}
	return n, err
}
