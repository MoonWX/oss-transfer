// Package ratelimit enforces shared node and parent byte budgets before source
// reads. Parent rates are this Agent's allocated share, not the whole cluster cap.
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

var (
	ErrStale    = errors.New("stale limit revision or generation")
	ErrConflict = errors.New("limit revision already has a different value")
	ErrMissing  = errors.New("parent bandwidth allocation is not installed")
	ErrStopped  = errors.New("parent bandwidth allocation is stopped")
	ErrClosed   = errors.New("bandwidth limiter is closed")
	ErrCapacity = errors.New("parent limit capacity reached")
)

const settingsBucket = "bandwidth_settings"
const parentsBucket = "bandwidth_parents"

// Keep at most one millisecond of earned credit at high rates. A single
// 64 KiB bucket discards credit whenever a sub-millisecond timer wakes late,
// even with a ready source and destination. This bounded window tolerates
// scheduler jitter without preloading tokens or changing the read-size bound.
const creditWindow = time.Millisecond
const maximumCreditBytes = 1 << 20

type NodeLimit struct {
	Revision                uint64 `json:"revision"`
	BandwidthBytesPerSecond int64  `json:"bandwidthBytesPerSecond"`
}
type ParentLimit struct {
	ListQPS                 float64 `json:"listQps,omitempty"`
	HeadQPS                 float64 `json:"headQps,omitempty"`
	PutQPS                  float64 `json:"putQps,omitempty"`
	ParentID                int64   `json:"parentId"`
	Generation              uint64  `json:"generation"`
	Revision                uint64  `json:"revision"`
	BandwidthBytesPerSecond int64   `json:"bandwidthBytesPerSecond"`
	Stopped                 bool    `json:"stopped"`
}
type Options struct {
	ChunkBytes int
	MaxParents int
}
type Database interface {
	View(func(*state.Tx) error) error
	Update(func(*state.Tx) error) error
}
type bucket struct {
	rate    int64
	tokens  float64
	last    time.Time
	started bool
}
type parentState struct {
	requestNext [3]time.Time
	limit       ParentLimit
	budget      bucket
	ctx         context.Context
	cancel      context.CancelCauseFunc
}

type Limiter struct {
	mu         sync.Mutex
	db         Database
	opts       Options
	node       NodeLimit
	nodeBudget bucket
	parents    map[int64]*parentState
	changed    chan struct{}
	closed     bool
}

func New(db Database, opts Options) (*Limiter, error) {
	if db == nil {
		return nil, errors.New("limit database is required")
	}
	if opts.ChunkBytes == 0 {
		opts.ChunkBytes = 64 << 10
	}
	if opts.MaxParents == 0 {
		opts.MaxParents = 100000
	}
	if opts.ChunkBytes < 1 || opts.ChunkBytes > 64<<10 || opts.MaxParents < 1 {
		return nil, errors.New("invalid limiter bounds")
	}
	l := &Limiter{db: db, opts: opts, parents: make(map[int64]*parentState), changed: make(chan struct{})}
	err := db.View(func(tx *state.Tx) error {
		ok, err := tx.Get(settingsBucket, "node", &l.node)
		if err != nil {
			return err
		}
		if ok {
			if err := validNode(l.node); err != nil {
				return err
			}
		}
		after := ""
		for {
			count := 0
			err := tx.Scan(parentsBucket, "", after, 256, func(key string, raw json.RawMessage) error {
				var p ParentLimit
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if err := validParent(p); err != nil {
					return err
				}
				if len(l.parents) >= opts.MaxParents {
					return ErrCapacity
				}
				l.parents[p.ParentID] = newParent(p)
				after = key
				count++
				return nil
			})
			if err != nil {
				return err
			}
			if count < 256 {
				return nil
			}
		}
	})
	if err != nil {
		l.Close()
		return nil, err
	}
	l.nodeBudget = bucket{rate: l.node.BandwidthBytesPerSecond, last: time.Now()}
	return l, nil
}

func newParent(p ParentLimit) *parentState {
	ctx, cancel := context.WithCancelCause(context.Background())
	if p.Stopped {
		cancel(ErrStopped)
	}
	return &parentState{limit: p, budget: bucket{rate: p.BandwidthBytesPerSecond, last: time.Now()}, ctx: ctx, cancel: cancel}
}
func validRate(rate int64) error {
	if rate < 0 || rate > 1<<50 {
		return errors.New("bandwidth bytes per second is out of range")
	}
	return nil
}
func validNode(n NodeLimit) error {
	if n.Revision == 0 {
		return errors.New("node revision must be positive")
	}
	return validRate(n.BandwidthBytesPerSecond)
}
func validParent(p ParentLimit) error {
	for _, value := range []float64{p.ListQPS, p.HeadQPS, p.PutQPS} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e9 || value > 0 && value < 1e-12 {
			return errors.New("invalid parent request allocation")
		}
	}
	if p.ParentID < 1 || p.Generation == 0 || p.Revision == 0 {
		return errors.New("parent identity, generation and revision must be positive")
	}
	return validRate(p.BandwidthBytesPerSecond)
}

// Apply methods publish the in-memory policy only after its durable commit.
// Revision equality is idempotent only when the entire value is identical.
func (l *Limiter) ApplyNode(ctx context.Context, n NodeLimit) error {
	if err := validNode(n); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.Revision < l.node.Revision {
		return ErrStale
	}
	if n.Revision == l.node.Revision {
		if n != l.node {
			return ErrConflict
		}
		return nil
	}
	if err := l.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Put(settingsBucket, "node", n)
	}); err != nil {
		return err
	}
	l.node = n
	l.nodeBudget = bucket{rate: n.BandwidthBytesPerSecond, last: time.Now()}
	l.notify()
	return nil
}

func (l *Limiter) ApplyParent(ctx context.Context, p ParentLimit) error {
	if err := validParent(p); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	old := l.parents[p.ParentID]
	if old != nil {
		if p.Generation < old.limit.Generation || (p.Generation == old.limit.Generation && p.Revision < old.limit.Revision) {
			return ErrStale
		}
		if p.Generation == old.limit.Generation && p.Revision == old.limit.Revision {
			if p != old.limit {
				return ErrConflict
			}
			return nil
		}
		if p.Generation == old.limit.Generation && old.limit.Stopped && !p.Stopped {
			return ErrStopped
		}
	} else if len(l.parents) >= l.opts.MaxParents {
		return ErrCapacity
	}
	if err := l.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Put(parentsBucket, fmt.Sprint(p.ParentID), p)
	}); err != nil {
		return err
	}
	if old == nil || old.limit.Generation != p.Generation {
		if old != nil {
			old.cancel(ErrStale)
		}
		l.parents[p.ParentID] = newParent(p)
	} else {
		for i, value := range []float64{p.ListQPS, p.HeadQPS, p.PutQPS} {
			previous := []float64{old.limit.ListQPS, old.limit.HeadQPS, old.limit.PutQPS}[i]
			if value != previous {
				old.requestNext[i] = time.Time{}
			}
		}
		old.limit = p
		old.budget = bucket{rate: p.BandwidthBytesPerSecond, last: time.Now()}
		if p.Stopped {
			old.cancel(ErrStopped)
		}
	}
	l.notify()
	return nil
}

func (l *Limiter) Node() NodeLimit { l.mu.Lock(); defer l.mu.Unlock(); return l.node }
func (l *Limiter) Parent(id int64) (ParentLimit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.parents[id]
	if p == nil {
		return ParentLimit{}, ErrMissing
	}
	return p.limit, nil
}
func (l *Limiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for _, p := range l.parents {
		p.cancel(ErrClosed)
	}
	l.notify()
}
func (l *Limiter) notify() { close(l.changed); l.changed = make(chan struct{}) }

func (l *Limiter) checkParent(id int64, generation uint64) (*parentState, error) {
	if l.closed {
		return nil, ErrClosed
	}
	p := l.parents[id]
	if p == nil {
		return nil, ErrMissing
	}
	if p.limit.Generation != generation {
		return nil, ErrStale
	}
	if p.limit.Stopped {
		return nil, ErrStopped
	}
	return p, nil
}

func (b *bucket) capacity(chunkBytes int) float64 {
	return math.Min(maximumCreditBytes, math.Max(float64(chunkBytes), float64(b.rate)*creditWindow.Seconds()))
}

func (b *bucket) refill(now time.Time, chunkBytes int) {
	if b.rate == 0 {
		return
	}
	// Configuration can precede the first transfer by hours. That idle time
	// must not grant a free initial burst to the first source reader.
	if !b.started {
		b.started = true
		b.last = now
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity(chunkBytes), b.tokens+elapsed*float64(b.rate))
		b.last = now
	}
}
func (b *bucket) delay(n int) time.Duration {
	if b.rate == 0 || b.tokens >= float64(n) {
		return 0
	}
	return time.Duration(math.Ceil((float64(n) - b.tokens) / float64(b.rate) * float64(time.Second)))
}
func (b *bucket) take(n int) {
	if b.rate > 0 {
		b.tokens -= float64(n)
	}
}

type reservation struct {
	size                                     int
	nodeRevision, parentRevision, generation uint64
	parentID                                 int64
}

func (l *Limiter) reserve(ctx context.Context, id int64, generation uint64, wanted int) (reservation, error) {
	for {
		if err := ctx.Err(); err != nil {
			return reservation{}, context.Cause(ctx)
		}
		l.mu.Lock()
		p, err := l.checkParent(id, generation)
		if err != nil {
			l.mu.Unlock()
			return reservation{}, err
		}
		n := min(wanted, l.opts.ChunkBytes)
		// Keep low-rate streams responsive: at most about 100 ms per read, and
		// never more than 64 KiB, even when rate changes during an existing wait.
		for _, rate := range []int64{l.nodeBudget.rate, p.budget.rate} {
			if rate > 0 {
				n = min(n, int(max(int64(1), min(int64(l.opts.ChunkBytes), rate/10))))
			}
		}
		now := time.Now()
		l.nodeBudget.refill(now, l.opts.ChunkBytes)
		p.budget.refill(now, l.opts.ChunkBytes)
		delay := max(l.nodeBudget.delay(n), p.budget.delay(n))
		if delay <= 0 {
			l.nodeBudget.take(n)
			p.budget.take(n)
			ticket := reservation{size: n, nodeRevision: l.node.Revision, parentRevision: p.limit.Revision, generation: generation, parentID: id}
			l.mu.Unlock()
			return ticket, nil
		}
		changed := l.changed
		l.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-ctx.Done():
			timer.Stop()
			return reservation{}, context.Cause(ctx)
		}
	}
}

// Short reads refund only the revision that granted the reservation. Refunding
// across an update would manufacture tokens under the new, lower cap.
func (l *Limiter) refund(ticket reservation, unused int) {
	if unused <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	credited := false
	if l.node.Revision == ticket.nodeRevision && l.nodeBudget.rate > 0 {
		l.nodeBudget.tokens = math.Min(l.nodeBudget.capacity(l.opts.ChunkBytes), l.nodeBudget.tokens+float64(unused))
		credited = true
	}
	if p := l.parents[ticket.parentID]; p != nil && p.limit.Generation == ticket.generation && p.limit.Revision == ticket.parentRevision && p.budget.rate > 0 {
		p.budget.tokens = math.Min(p.budget.capacity(l.opts.ChunkBytes), p.budget.tokens+float64(unused))
		credited = true
	}
	if credited && !l.closed {
		l.notify()
	}
}
