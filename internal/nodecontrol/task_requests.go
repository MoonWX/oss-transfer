package nodecontrol

import (
	"context"
	"errors"
	"time"
)

type parentRequests interface {
	TryRequest(int64, uint64, string, time.Time) (time.Duration, <-chan struct{}, error)
}

// BeforeRequest admits a real attempt jointly against the node and parent
// budgets. Acquisition is not split into two independent sleeping queues.
func (c *Controller) BeforeRequest(ctx context.Context, info RequestContext, nodeKind, taskKind string) error {
	index := kindIndex(nodeKind)
	if nodeKind != "" && (index < 0 || index == 3) {
		return ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if err := c.healthy(); err != nil {
			c.mu.Unlock()
			return err
		}
		now := time.Now()
		rate := c.data.Effective[nodeKind]
		var notBefore time.Time
		if nodeKind != "" && rate > 0 {
			notBefore = c.next[nodeKind]
		}
		delay := notBefore.Sub(now)
		var parentChanged <-chan struct{}
		if info.TaskID > 0 && info.Generation > 0 && taskKind != "" {
			parents, ok := c.bytes.(parentRequests)
			if !ok {
				c.mu.Unlock()
				return errors.New("parent request limiter is unavailable")
			}
			var err error
			delay, parentChanged, err = parents.TryRequest(info.TaskID, info.Generation, taskKind, notBefore)
			if err != nil {
				c.mu.Unlock()
				return err
			}
		}
		if delay <= 0 {
			if nodeKind != "" {
				if rate > 0 {
					c.next[nodeKind] = time.Now().Add(time.Duration(float64(time.Second) / rate))
				}
				c.addSample(index, 1)
			}
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-parentChanged:
			timer.Stop()
		case <-timer.C:
		}
	}
}
