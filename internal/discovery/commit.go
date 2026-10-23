package discovery

import (
	"context"
	"time"
)

const scanCommitBatches = 32
const scanCommitInterval = 20 * time.Millisecond
const scanCommitMetadataBytes = 4 << 20

// Keep the public 128-object batches and their durable sequence numbers, but
// share the disk transaction for bursts of scan output. The first batch remains
// synchronous so transfer can start immediately. The one-slot queue bounds memory
// and propagates disk backpressure to the source listing. Metadata, not payload
// bytes, bounds a group; one already formed batch can exceed that bound and is
// committed alone without changing its public sequence number.
type scanCommitter struct {
	ctx    context.Context
	cancel context.CancelFunc
	commit func([]pendingBatch) error
	queue  chan pendingBatch
	done   chan struct{}
	first  bool
	closed bool  // add/close have one producer, the source scan
	err    error // written by loop, read only after done closes
}

func newScanCommitter(ctx context.Context, cancel context.CancelFunc, commit func([]pendingBatch) error) *scanCommitter {
	c := &scanCommitter{ctx: ctx, cancel: cancel, commit: commit, queue: make(chan pendingBatch, 1), done: make(chan struct{}), first: true}
	go c.loop()
	return c
}

func (c *scanCommitter) add(batch pendingBatch) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.first {
		c.first = false
		return c.commit([]pendingBatch{batch})
	}
	select {
	case c.queue <- batch:
		return nil
	case <-c.done:
		return c.err
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *scanCommitter) close() error {
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	<-c.done
	return c.err
}

func (c *scanCommitter) loop() {
	defer close(c.done)
	var carry *pendingBatch
	for {
		var first pendingBatch
		if carry != nil {
			first, carry = *carry, nil
		} else {
			var ok bool
			first, ok = <-c.queue
			if !ok {
				return
			}
		}
		batch := []pendingBatch{first}
		metadataBytes := batchMetadataBytes(first)
		timer := time.NewTimer(scanCommitInterval)
		closed := false
	collect:
		for len(batch) < scanCommitBatches && metadataBytes < scanCommitMetadataBytes {
			select {
			case next, ok := <-c.queue:
				if !ok {
					closed = true
					break collect
				}
				nextBytes := batchMetadataBytes(next)
				if nextBytes > scanCommitMetadataBytes-metadataBytes {
					carry = &next
					break collect
				}
				batch = append(batch, next)
				metadataBytes += nextBytes
			case <-timer.C:
				break collect
			case <-c.ctx.Done():
				break collect
			}
		}
		timer.Stop()
		if err := c.commit(batch); err != nil {
			c.err = err
			c.cancel()
			return
		}
		if closed {
			return
		}
	}
}

// Conservatively account for JSON escaping and field labels without allocating
// a second encoded copy of every object. Content size does not consume memory
// here: the scanner stores identity metadata and never reads object bodies.
func batchMetadataBytes(batch pendingBatch) int64 {
	var size int64
	for _, object := range batch.objects {
		size += 512
		for _, field := range [...]string{object.Key, object.ETag, object.VersionID, object.ContentType,
			object.ContentEncoding, object.ContentDisposition, object.CacheControl, object.StorageClass} {
			size += 6 * int64(len(field))
		}
		for key, value := range object.Metadata {
			size += 16 + 6*int64(len(key)) + 6*int64(len(value))
		}
	}
	return size
}
