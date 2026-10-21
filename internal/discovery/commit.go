package discovery

import (
	"context"
	"time"
)

const scanCommitBatches = 8
const scanCommitInterval = 20 * time.Millisecond

// Keep the public 128-object batches and their durable sequence numbers, but
// share the disk transaction for bursts of scan output. The first batch remains
// synchronous so transfer can start immediately. The one-slot queue bounds memory
// and propagates disk backpressure to the source listing.
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
	for first := range c.queue {
		batch := []pendingBatch{first}
		timer := time.NewTimer(scanCommitInterval)
		closed := false
	collect:
		for len(batch) < scanCommitBatches {
			select {
			case next, ok := <-c.queue:
				if !ok {
					closed = true
					break collect
				}
				batch = append(batch, next)
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
