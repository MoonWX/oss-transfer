package transferstore

import (
	"context"
	"sync"
)

// maxPartSlots bounds one object's concurrent ranged parts, including the
// guaranteed PartConcurrency slots and any borrowed ones.
const maxPartSlots = 16

// PartLender lends idle node object capacity to ranged multipart uploads and
// ranged local writes. An idle object slot is worth one object's base part
// slots. Lending never blocks and never reserves object slots, so newly
// admitted objects are not delayed; they only make fewer slots idle for the
// next borrow.
type PartLender struct {
	mu   sync.Mutex
	idle func() int
	lent int
}

func NewPartLender() *PartLender { return &PartLender{} }

// SetIdle installs the node's idle object-slot count. It must be cheap and
// non-blocking, because it is consulted before each borrowed part.
func (l *PartLender) SetIdle(idle func() int) {
	l.mu.Lock()
	l.idle = idle
	l.mu.Unlock()
}

// Lent reports borrowed part slots that have not yet been returned.
func (l *PartLender) Lent() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lent
}

func (l *PartLender) tryBorrow(basePerObject int) (func(), bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idle == nil || l.lent >= l.idle()*basePerObject {
		return nil, false
	}
	l.lent++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.lent--
			l.mu.Unlock()
		})
	}, true
}

// rangedSlots admits one object's concurrent ranges: perObject guaranteed
// slots, plus slots borrowed from the PartLender while the node is idle, up to
// maxPartSlots in total. Borrowed slots return when their range ends. When
// memory is set, every range also holds its length in that budget: base slots
// wait for it in FIFO order, borrowed slots only take memory that is free.
type rangedSlots struct {
	lender    *PartLender
	perObject int
	memory    *UploadMemoryBudget
	base      chan struct{}
	finished  chan struct{}
	mu        sync.Mutex
	borrowed  int
}

func newRangedSlots(lender *PartLender, perObject, count int, memory *UploadMemoryBudget) *rangedSlots {
	return &rangedSlots{lender: lender, perObject: perObject, memory: memory, base: make(chan struct{}, max(1, min(perObject, count))), finished: make(chan struct{}, 1)}
}

// done wakes a dispatcher waiting for a slot to retry borrowing.
func (r *rangedSlots) done() {
	select {
	case r.finished <- struct{}{}:
	default:
	}
}

func (r *rangedSlots) acquire(ctx context.Context, length int64) (func(), error) {
	for {
		select {
		case r.base <- struct{}{}:
			return r.baseMemory(ctx, length)
		default:
		}
		if release, ok := r.borrow(length); ok {
			return release, nil
		}
		select {
		case r.base <- struct{}{}:
			return r.baseMemory(ctx, length)
		case <-r.finished:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *rangedSlots) baseMemory(ctx context.Context, length int64) (func(), error) {
	if r.memory == nil {
		if err := ctx.Err(); err != nil {
			<-r.base
			return nil, err
		}
		return func() { <-r.base }, nil
	}
	release, err := r.memory.acquire(ctx, length)
	if err != nil {
		<-r.base
		return nil, err
	}
	return func() { release(); <-r.base }, nil
}

func (r *rangedSlots) borrow(length int64) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cap(r.base)+r.borrowed >= maxPartSlots {
		return nil, false
	}
	returnSlot, ok := r.lender.tryBorrow(r.perObject)
	if !ok {
		return nil, false
	}
	releaseMemory := func() {}
	if r.memory != nil {
		if releaseMemory, ok = r.memory.tryAcquire(length, r.memory.capacity/4); !ok {
			returnSlot()
			return nil, false
		}
	}
	r.borrowed++
	return func() {
		releaseMemory()
		returnSlot()
		r.mu.Lock()
		r.borrowed--
		r.mu.Unlock()
	}, true
}
