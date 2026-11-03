package transferstore

import "sync"

// maxPartSlots bounds one object's concurrent ranged parts, including the
// guaranteed PartConcurrency slots and any borrowed ones.
const maxPartSlots = 16

// PartLender lends idle node object capacity to ranged multipart uploads. An
// idle object slot is worth one object's base part slots. Lending never blocks
// and never reserves object slots, so newly admitted objects are not delayed;
// they only make fewer slots idle for the next borrow.
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
