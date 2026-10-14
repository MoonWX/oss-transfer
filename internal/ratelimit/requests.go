package ratelimit

import (
	"errors"
	"math"
	"time"
)

// TryRequest admits one HTTP attempt only when both the parent share and the
// caller's external (node) deadline permit it. Sleeping callers reserve nothing.
// The caller holds its node mutex until it commits that node's permit, preventing
// a queue of previously acquired parent permits from bursting after a node wait.
func (l *Limiter) TryRequest(parentID int64, generation uint64, kind string, notBefore time.Time) (time.Duration, <-chan struct{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := l.checkParent(parentID, generation)
	if err != nil {
		return 0, l.changed, err
	}
	index := -1
	var rate float64
	switch kind {
	case "list":
		index, rate = 0, p.limit.ListQPS
	case "head":
		index, rate = 1, p.limit.HeadQPS
	case "put":
		index, rate = 2, p.limit.PutQPS
	default:
		return 0, l.changed, errors.New("invalid request kind")
	}
	now := time.Now()
	interval := time.Duration(0)
	if rate > 0 {
		seconds := math.Ceil(float64(time.Second) / rate)
		if seconds >= float64(math.MaxInt64) {
			interval = time.Duration(math.MaxInt64)
		} else {
			interval = time.Duration(seconds)
		}
		if p.requestNext[index].IsZero() {
			p.requestNext[index] = now.Add(interval)
		}
	}
	delay := notBefore.Sub(now)
	if rate > 0 {
		delay = max(delay, p.requestNext[index].Sub(now))
	}
	if delay > 0 {
		return delay, l.changed, nil
	}
	if rate > 0 {
		p.requestNext[index] = now.Add(interval)
	}
	return 0, l.changed, nil
}
