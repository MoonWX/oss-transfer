package transferstore

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

const DefaultUploadMemoryBytes int64 = 256 << 20

// UploadMemoryBudget bounds resident upload payloads across all stores on an
// Agent, including single PUTs and multipart retries. It does not bound other
// process allocations, such as SDK metadata, the task index or HTTP buffers.
type UploadMemoryBudget struct {
	mu             sync.Mutex
	capacity, used int64
	waiting        list.List
}

type uploadReservation struct {
	bytes   int64
	ready   chan struct{}
	granted bool
}

func NewUploadMemoryBudget(bytes int64) (*UploadMemoryBudget, error) {
	if bytes <= 0 {
		return nil, errors.New("upload memory budget must be positive")
	}
	return &UploadMemoryBudget{capacity: bytes}, nil
}

// acquire queues a whole part before its first allocation/read. Each uploaded
// part releases independently, so callers never wait for all parts to be in
// memory. FIFO admission prevents small objects starving a larger waiting part.
func (b *UploadMemoryBudget) acquire(ctx context.Context, bytes int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b == nil || bytes == 0 {
		return func() {}, nil
	}
	if bytes < 0 || bytes > b.capacity {
		return nil, errors.New("upload part exceeds the node upload memory budget")
	}
	r := &uploadReservation{bytes: bytes, ready: make(chan struct{})}
	b.mu.Lock()
	element := b.waiting.PushBack(r)
	b.admit()
	b.mu.Unlock()
	select {
	case <-r.ready:
		if err := ctx.Err(); err != nil {
			b.release(bytes)
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { b.release(bytes) }) }, nil
	case <-ctx.Done():
		b.mu.Lock()
		if r.granted {
			b.used -= bytes
		} else {
			b.waiting.Remove(element)
		}
		b.admit()
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (b *UploadMemoryBudget) admit() {
	for front := b.waiting.Front(); front != nil; front = b.waiting.Front() {
		r := front.Value.(*uploadReservation)
		if r.bytes > b.capacity-b.used {
			return
		}
		b.waiting.Remove(front)
		b.used += r.bytes
		r.granted = true
		close(r.ready)
	}
}

func (b *UploadMemoryBudget) release(bytes int64) {
	b.mu.Lock()
	b.used -= bytes
	b.admit()
	b.mu.Unlock()
}
