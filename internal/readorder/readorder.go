// Package readorder lets the concurrent source reads of one ranged write wait
// for bandwidth in offset order. A ranged write sets a Turn in the context of
// each range it fetches. A bandwidth limiter waits for a read's tokens inside
// Turn.Serve, and the read uses its source only after Serve returns.
//
// Without an order, a limiter shares its rate evenly among the waiting ranges,
// so a window of equal chunks that started together also finishes together.
// A writer that appends chunks in offset order then receives the whole window
// at once and writes it while the network has nothing left to fetch. Serving
// the lowest waiting range first, and letting it take over from a higher
// range that is still waiting for tokens, makes chunks complete one after
// another in the order they are written. Only the wait is ordered: a range
// whose connection has no data yet holds no turn, so later ranges still take
// any rate the earlier ones cannot.
//
// A range passed over for MaxPassedOver is served next, whatever its rank.
// Its request is already open, and servers and proxies close a response they
// cannot send for about a minute (nginx send_timeout, Ceph RGW's request
// timeout, load balancer idle timeouts), which would cost the range a second
// GET.
package readorder

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// MaxPassedOver bounds how long a waiting range can be passed over by lower
// ranks: well under the common one-minute send timeouts.
const MaxPassedOver = 10 * time.Second

// ErrPreempted is what a reserve function passed to Serve returns, having
// taken nothing, once its yield channel is closed.
var ErrPreempted = errors.New("a lower range took over the bandwidth wait")

// Queue orders the turns of one ranged write. At most one turn holds it at a
// time.
type Queue struct {
	mu      sync.Mutex
	holder  *waiter // nil while the queue is free
	waiting []*waiter
}

type waiter struct {
	rank      int64
	since     time.Time     // when this read began waiting
	ready     chan struct{} // closed when the read is handed the queue
	yield     chan struct{} // closed when a lower rank takes over; nil if none may
	preempted bool
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

// Serve calls reserve while t holds its queue and returns what reserve
// returns. When a lower rank starts waiting, reserve's yield channel closes;
// reserve must then return ErrPreempted without taking anything, and Serve
// waits for the queue again, keeping the time it began waiting. A read that
// cannot be preempted gets a nil channel. The zero Turn calls reserve(nil).
func (t Turn) Serve(ctx context.Context, reserve func(yield <-chan struct{}) error) error {
	q := t.queue
	if q == nil {
		return reserve(nil)
	}
	w := &waiter{rank: t.rank, since: time.Now()}
	for {
		yield, err := q.acquire(ctx, w)
		if err != nil {
			return err
		}
		err = reserve(yield)
		q.mu.Lock()
		q.handOff()
		q.mu.Unlock()
		if !errors.Is(err, ErrPreempted) {
			return err
		}
	}
}

// acquire waits until w holds the queue and returns w's yield channel. On
// cancellation it leaves the queue, or passes it on if it was just handed it.
func (q *Queue) acquire(ctx context.Context, w *waiter) (<-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	q.mu.Lock()
	if q.holder == nil {
		q.grant(w, true) // a free queue has no waiters
		yield := w.yield
		q.mu.Unlock()
		return yield, nil
	}
	w.ready = make(chan struct{})
	q.waiting = append(q.waiting, w)
	if h := q.holder; h.yield != nil && !h.preempted && w.rank < h.rank {
		h.preempted = true
		close(h.yield)
	}
	q.mu.Unlock()
	select {
	case <-w.ready:
		return w.yield, nil
	case <-ctx.Done():
		q.mu.Lock()
		if q.holder == w {
			q.handOff()
		} else {
			i := slices.Index(q.waiting, w)
			q.waiting = slices.Delete(q.waiting, i, i+1)
		}
		q.mu.Unlock()
		return nil, context.Cause(ctx)
	}
}

// handOff passes the queue to the next waiter, or frees it. A waiter passed
// over for MaxPassedOver goes first, longest waiting first, and cannot be
// preempted; otherwise the lowest rank does. The caller holds q.mu.
func (q *Queue) handOff() {
	q.holder = nil
	if len(q.waiting) == 0 {
		return
	}
	oldest, lowest := 0, 0
	for i, w := range q.waiting {
		if w.since.Before(q.waiting[oldest].since) {
			oldest = i
		}
		if w.rank < q.waiting[lowest].rank {
			lowest = i
		}
	}
	next, passedOver := lowest, time.Since(q.waiting[oldest].since) >= MaxPassedOver
	if passedOver {
		next = oldest
	}
	w := q.waiting[next]
	q.waiting = slices.Delete(q.waiting, next, next+1)
	q.grant(w, !passedOver)
	close(w.ready)
}

// grant makes w the holder. The caller holds q.mu.
func (q *Queue) grant(w *waiter, preemptible bool) {
	q.holder = w
	w.preempted = false
	w.yield = nil
	if preemptible {
		w.yield = make(chan struct{})
	}
}
