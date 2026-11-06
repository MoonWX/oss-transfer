// Package readorder lets the concurrent source reads of one ranged write wait
// for bandwidth in offset order. A ranged write sets a Turn in the context of
// each range it fetches; a bandwidth limiter holds that Turn while the read
// waits for tokens, and not while it reads.
//
// Without an order, a limiter shares its rate evenly among the waiting ranges,
// so a window of equal chunks that started together also finishes together.
// A writer that appends chunks in offset order then receives the whole window
// at once and writes it while the network has nothing left to fetch. Handing
// tokens to the lowest waiting range first makes chunks complete one after
// another, in the order they are written. Only the wait is ordered: a range
// whose connection has no data yet holds no turn, so later ranges still take
// any rate the earlier ones cannot.
package readorder

import (
	"context"
	"slices"
	"sync"
)

// Queue orders the turns of one ranged write. At most one of its turns is
// held at a time, and a released queue passes to the lowest waiting rank.
type Queue struct {
	mu      sync.Mutex
	held    bool // a turn holds the queue, or was handed it and has not woken
	waiting []*waiter
}

type waiter struct {
	rank    int64
	ready   chan struct{}
	granted bool
}

func NewQueue() *Queue { return &Queue{} }

// Turn is one read's place in a Queue. The zero Turn never waits.
type Turn struct {
	queue *Queue
	rank  int64
}

type contextKey struct{}

// With returns ctx carrying the turn of the read ranked rank in queue; a
// lower rank is served first.
func With(ctx context.Context, queue *Queue, rank int64) context.Context {
	return context.WithValue(ctx, contextKey{}, Turn{queue: queue, rank: rank})
}

// From returns the turn carried by ctx, or the zero Turn.
func From(ctx context.Context) Turn {
	turn, _ := ctx.Value(contextKey{}).(Turn)
	return turn
}

// Wait blocks until t holds its queue. On success the caller must call Done
// exactly once; on cancellation the queue is passed on and Wait returns the
// context's cause.
func (t Turn) Wait(ctx context.Context) error {
	q := t.queue
	if q == nil {
		return nil
	}
	q.mu.Lock()
	if !q.held {
		q.held = true // a free queue has no waiters
		q.mu.Unlock()
		return nil
	}
	w := &waiter{rank: t.rank, ready: make(chan struct{})}
	q.waiting = append(q.waiting, w)
	q.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		granted := w.granted
		if !granted {
			i := slices.Index(q.waiting, w)
			q.waiting = slices.Delete(q.waiting, i, i+1)
		}
		q.mu.Unlock()
		if granted {
			q.release()
		}
		return context.Cause(ctx)
	}
}

// Done releases a turn held after a successful Wait.
func (t Turn) Done() {
	if t.queue != nil {
		t.queue.release()
	}
}

func (q *Queue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handOff()
}

// handOff passes a released queue to the lowest waiting rank, or frees it.
// The caller holds q.mu.
func (q *Queue) handOff() {
	if len(q.waiting) == 0 {
		q.held = false
		return
	}
	next := 0
	for i, w := range q.waiting {
		if w.rank < q.waiting[next].rank {
			next = i
		}
	}
	w := q.waiting[next]
	q.waiting = slices.Delete(q.waiting, next, next+1)
	w.granted = true
	close(w.ready)
}
