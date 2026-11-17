package nodecontrol

import (
	"context"
	"time"
)

// WaitLocalOperation admits one local file operation (an open, a create, a
// metadata lookup or a directory read) under the node's local operation limit.
func (c *Controller) WaitLocalOperation(ctx context.Context) error {
	return c.Wait(ctx, "localOps")
}

// Local bandwidth may run ahead of its average by a short credit after a pause
// (a slow write on the other side, for example), as node bandwidth does: 100 ms
// of the limit, at least 64 KiB and at most 16 MiB.
const (
	localCreditDuration = 100 * time.Millisecond
	localCreditMin      = 64 << 10
	localCreditMax      = 16 << 20
)

// WaitLocalBytes admits n bytes read from or written to local files under the
// node's local bandwidth limit. Like Wait, it reserves nothing while it sleeps,
// so a cancelled caller leaves no debt for the others.
func (c *Controller) WaitLocalBytes(ctx context.Context, n int) error {
	if n <= 0 {
		return ctx.Err()
	}
	const kind = "localBandwidth"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if err := c.healthy(); err != nil {
			c.mu.Unlock()
			return err
		}
		bytesPerSecond := c.data.Effective[kind] / 8
		now := time.Now()
		if bytesPerSecond <= 0 {
			c.addSample(kindIndex(kind), int64(n))
			c.mu.Unlock()
			return nil
		}
		credit := min(max(bytesPerSecond*localCreditDuration.Seconds(), localCreditMin), localCreditMax)
		earliest := now.Add(-time.Duration(credit / bytesPerSecond * float64(time.Second)))
		next := c.next[kind]
		if next.Before(earliest) {
			next = earliest
		}
		if !next.After(now) {
			c.next[kind] = next.Add(time.Duration(float64(n) / bytesPerSecond * float64(time.Second)))
			c.addSample(kindIndex(kind), int64(n))
			c.mu.Unlock()
			return nil
		}
		delay := next.Sub(now)
		changed := c.changed
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
