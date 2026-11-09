package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// rangeReader gives each source range the same decorations as a streamed
// object: node and task bandwidth, observed bytes and live progress. The
// decorator sees the range length as the item size, so limiter EOF checks
// apply per range.
func (e *Engine) rangeReader(job Job, item Item, source transferstore.RangeSource, object *objectProgress) transferstore.RangeFunc {
	return func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		input, err := source.OpenRange(ctx, offset, length)
		if err != nil {
			return nil, markSource(err)
		}
		stop := context.AfterFunc(ctx, func() { _ = input.Close() })
		var reader io.ReadCloser = input
		if e.opts.DecorateReader != nil {
			part := item
			part.Source.Size = length
			reader, err = e.opts.DecorateReader(ctx, job, part, input)
			if err == nil && reader == nil {
				err = errors.New("nil decorated reader")
			}
			if err != nil {
				stop()
				_ = input.Close()
				return nil, fmt.Errorf("prepare reader: %w", err)
			}
		}
		return &progressRange{reader: reader, input: input, stop: stop, progress: object, length: length}, nil
	}
}

// progressRange counts bytes as they are read, like progressReader. A range
// closed before it delivered exactly length bytes and a clean EOF withdraws
// its bytes, so a repeated range read is never counted twice.
type progressRange struct {
	reader, input io.ReadCloser
	stop          func() bool
	progress      *objectProgress
	length        int64
	counted       atomic.Int64
	eof           atomic.Bool
	once          sync.Once
	err           error
}

func (r *progressRange) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	err = markSource(err)
	if n > 0 && r.progress != nil {
		r.counted.Add(int64(n))
		r.progress.bytes.Add(int64(n))
	}
	if errors.Is(err, io.EOF) {
		r.eof.Store(true)
	}
	return n, err
}

func (r *progressRange) Close() error {
	r.once.Do(func() {
		r.stop()
		r.err = r.reader.Close()
		if r.reader != r.input {
			_ = r.input.Close()
		}
		if counted := r.counted.Load(); (counted != r.length || !r.eof.Load()) && r.progress != nil {
			r.progress.bytes.Add(-counted)
		}
	})
	return r.err
}

// lazyReader opens its source on first Read, so an unused fallback costs no
// request. Close before the first Read is a no-op.
type lazyReader struct {
	open   func() (io.ReadCloser, error)
	mu     sync.Mutex
	reader io.ReadCloser
	closed bool
}

func (r *lazyReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, context.Canceled
	}
	if r.reader == nil {
		reader, err := r.open()
		if err != nil {
			r.mu.Unlock()
			return 0, err
		}
		r.reader = reader
	}
	reader := r.reader
	r.mu.Unlock()
	return reader.Read(buffer)
}

func (r *lazyReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.reader == nil {
		return nil
	}
	return r.reader.Close()
}
