package transferstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	defaultLocalPartConcurrency = 4
	// Local ranges stream straight into the file, so their length costs no
	// memory. Aim for 64 ranges per object, so sixteen slots stay busy, while
	// keeping each range between 16 MiB and 64 MiB: large enough to bound the
	// request count, small enough that a retried range repeats little.
	localRangedTargetParts int64 = 64
	localRangedMinPart     int64 = 16 << 20
	localRangedMaxPart     int64 = 64 << 20
	localCopyBuffer              = 256 << 10
)

// LocalRangedOptions controls ranged writes, which fetch a large object
// through WriteOptions.Ranges in concurrent byte ranges instead of one stream.
type LocalRangedOptions struct {
	// MinSize is the smallest object written in ranges: 0 means 128 MiB, as
	// for S3 ranged uploads; a negative value disables ranged writes.
	MinSize int64
	// PartSize fixes the range length; 0 derives 16 to 64 MiB from the size.
	PartSize int64
	// PartConcurrency is the guaranteed ranges per object: 0 means 4, at most 16.
	PartConcurrency int
	// PartLender lends extra range slots while the node is idle, up to 16 per
	// object. Nil disables borrowing.
	PartLender *PartLender
}

// SetRangedWrites must be called before the Local is used by any goroutine.
func (s *Local) SetRangedWrites(options LocalRangedOptions) error {
	if options.MinSize == 0 {
		options.MinSize = defaultRangedMinSize
	}
	if options.PartConcurrency == 0 {
		options.PartConcurrency = defaultLocalPartConcurrency
	}
	if options.PartConcurrency < 1 || options.PartConcurrency > maxPartSlots {
		return errors.New("local part concurrency must be between 1 and 16")
	}
	if options.PartSize < 0 {
		return errors.New("local part size must not be negative")
	}
	s.ranged = options
	return nil
}

func (s *Local) rangedPartSize(size int64) int64 {
	if s.ranged.PartSize > 0 {
		return s.ranged.PartSize
	}
	part := (size + localRangedTargetParts - 1) / localRangedTargetParts
	part = (part + (1 << 20) - 1) &^ ((1 << 20) - 1)
	return min(max(part, localRangedMinPart), localRangedMaxPart)
}

// RangedWrite reports whether Write fetches a file of this size through
// WriteOptions.Ranges. Only objects of more than one range qualify.
func (s *Local) RangedWrite(size int64) bool {
	return s.ranged.MinSize >= 0 && size >= s.ranged.MinSize && size > s.rangedPartSize(size)
}

// writeRanges fills a file already sized to size. Each slot streams one source
// range into its own region with WriteAt, so no range is buffered whole and
// ranges may finish in any order. The first failure cancels every other range
// reader; writeRanges returns only after all of its goroutines have stopped,
// so no write can reach the file after the caller closes it.
func (s *Local) writeRanges(parent context.Context, file *os.File, ranges RangeFunc, size int64) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	partSize := s.rangedPartSize(size)
	count := (size + partSize - 1) / partSize
	slots := newRangedSlots(s.ranged.PartLender, s.ranged.PartConcurrency, int(min(count, maxPartSlots)), nil)
	var group sync.WaitGroup
	var failure sync.Once
	var firstError error
	fail := func(err error) {
		if err != nil {
			failure.Do(func() { firstError = err; cancel() })
		}
	}
	buffers := sync.Pool{New: func() any { buffer := make([]byte, localCopyBuffer); return &buffer }}
	for index := range count {
		offset := index * partSize
		length := min(partSize, size-offset)
		release, err := slots.acquire(ctx, length)
		if err != nil {
			fail(err)
			break
		}
		group.Add(1)
		go func() {
			defer group.Done()
			defer slots.done()
			defer release()
			buffer := buffers.Get().(*[]byte)
			defer buffers.Put(buffer)
			fail(writeRange(ctx, ranges, file, offset, length, *buffer))
		}()
	}
	group.Wait()
	if firstError != nil {
		return firstError
	}
	return parent.Err()
}

// writeRange repeats a range whose body was interrupted with a fresh
// conditional read of the whole range; it rewrites the same region with the
// same pinned bytes. A changed source, a length mismatch, a write error and
// cancellation are final.
func writeRange(ctx context.Context, ranges RangeFunc, file *os.File, offset, length int64, buffer []byte) error {
	for attempt := 1; ; attempt++ {
		err := copyRange(ctx, ranges, file, offset, length, buffer)
		var interrupted rangeBodyError
		if err == nil || attempt >= rangeReadAttempts || ctx.Err() != nil || !errors.As(err, &interrupted) {
			return err
		}
	}
}

// copyRange reads exactly length bytes and then a clean EOF, writing each
// chunk at its offset as soon as it arrives. Like readRange it keeps a short
// source apart from a truncated body, which is worth another read.
func copyRange(ctx context.Context, ranges RangeFunc, file *os.File, offset, length int64, buffer []byte) error {
	input, err := ranges(ctx, offset, length)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("range source returned a nil reader")
	}
	stop := interruptInput(ctx, input)
	defer stop()
	defer input.Close()
	reader := contextReader{ctx, input}
	for written := int64(0); written < length; {
		n, readErr := reader.Read(buffer[:min(int64(len(buffer)), length-written)])
		if n > 0 {
			if _, err := file.WriteAt(buffer[:n], offset+written); err != nil {
				return fmt.Errorf("write local range: %w", err)
			}
			written += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			if written < length {
				return fmt.Errorf("%w: source range shorter than declared length", ErrSizeMismatch)
			}
			break
		}
		if readErr != nil {
			return rangeReadError(ctx, readErr)
		}
	}
	return rangeReadError(ctx, requireEOF(reader))
}
