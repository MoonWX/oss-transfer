package transferstore

import (
	"context"
	"errors"
	"io"
	"sync"
)

const (
	defaultLocalPartConcurrency = 4
	// 8 MiB chunks keep an object's reorder window small (four guaranteed
	// chunks are 32 MiB, sixteen are 128 MiB) while a 4 GiB object still
	// needs only 512 range requests.
	defaultLocalChunkSize int64 = 8 << 20
	localCopyBuffer             = 256 << 10
)

// LocalRangedOptions controls ranged writes, which fetch a large object
// through WriteOptions.Ranges in concurrent byte ranges instead of one
// stream. The file itself is always written sequentially, in offset order.
type LocalRangedOptions struct {
	// MinSize is the smallest object written in ranges: 0 means 128 MiB, as
	// for S3 ranged uploads; a negative value disables ranged writes.
	MinSize int64
	// ChunkSize is the length of each range; 0 means 8 MiB.
	ChunkSize int64
	// PartConcurrency is the guaranteed ranges per object: 0 means 4, at most 16.
	PartConcurrency int
	// PartLender lends extra range slots while the node is idle, up to 16 per
	// object. Nil disables borrowing.
	PartLender *PartLender
	// Memory holds every fetched chunk until it has been written. Ranged
	// writes are disabled without it, so their memory is always bounded.
	Memory *UploadMemoryBudget
}

// SetRangedWrites must be called before the Local is used by any goroutine.
func (s *Local) SetRangedWrites(options LocalRangedOptions) error {
	if options.MinSize == 0 {
		options.MinSize = defaultRangedMinSize
	}
	if options.ChunkSize == 0 {
		options.ChunkSize = defaultLocalChunkSize
	}
	if options.PartConcurrency == 0 {
		options.PartConcurrency = defaultLocalPartConcurrency
	}
	if options.PartConcurrency < 1 || options.PartConcurrency > maxPartSlots {
		return errors.New("local part concurrency must be between 1 and 16")
	}
	if options.ChunkSize < 0 || options.ChunkSize > maxMemoryPart {
		return errors.New("local chunk size must be between 1 byte and 64 MiB")
	}
	s.ranged = options
	return nil
}

// RangedWrite reports whether Write fetches a file of this size through
// WriteOptions.Ranges: it must span more than one chunk, and a chunk must fit
// the memory budget.
func (s *Local) RangedWrite(size int64) bool {
	r := s.ranged
	return r.MinSize >= 0 && r.Memory != nil && r.ChunkSize > 0 && size >= r.MinSize && size > r.ChunkSize && r.ChunkSize <= r.Memory.capacity
}

// writeRanges fetches chunks concurrently and writes them strictly in offset
// order, so the file only ever grows by sequential appends: network and FUSE
// filesystems that are slow at scattered writes see one ordinary stream.
//
// A chunk holds its slot and its memory from the start of its fetch until it
// has been written. That bounds the reorder window to the object's slots (4
// guaranteed, up to 16 borrowed) and their memory to the shared budget; when
// the writer falls behind, the window fills and fetching pauses. Chunks are
// dispatched in order, so the chunk the writer needs next always already
// holds its slot and memory: the window can never fill with later chunks only.
//
// The first failure cancels every fetch and the writer. writeRanges returns
// only after all of its goroutines have stopped and every reservation has
// been released, so nothing touches w after it returns.
func (s *Local) writeRanges(parent context.Context, w io.Writer, ranges RangeFunc, size int64) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	chunkSize := s.ranged.ChunkSize
	count := (size + chunkSize - 1) / chunkSize
	slots := newRangedSlots(s.ranged.PartLender, s.ranged.PartConcurrency, int(min(count, maxPartSlots)), s.ranged.Memory)
	var failure sync.Once
	var firstError error
	fail := func(err error) {
		if err != nil {
			failure.Do(func() { firstError = err; cancel() })
		}
	}
	buffers := sync.Pool{New: func() any { buffer := make([]byte, chunkSize); return &buffer }}
	order := newChunkOrder(count)
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		fail(order.write(ctx, w, &buffers))
	}()
	var fetchers sync.WaitGroup
	for index := range count {
		offset := index * chunkSize
		length := min(chunkSize, size-offset)
		reservation, err := slots.acquire(ctx, length)
		if err != nil {
			fail(err)
			break
		}
		// A slot frees when its chunk is written (or abandoned), not when its
		// fetch ends; wake the dispatcher then so it can borrow again.
		var once sync.Once
		release := func() { once.Do(func() { reservation(); slots.done() }) }
		fetchers.Add(1)
		go func() {
			defer fetchers.Done()
			buffer := buffers.Get().(*[]byte)
			data := (*buffer)[:length]
			if err := readRangeRetrying(ctx, ranges, offset, data); err != nil {
				buffers.Put(buffer)
				release()
				fail(err)
				return
			}
			order.deliver(index, &fetchedChunk{data: data, buffer: buffer, release: release})
		}()
	}
	fetchers.Wait()
	<-writer
	order.close(&buffers)
	if firstError != nil {
		return firstError
	}
	return parent.Err()
}

type fetchedChunk struct {
	data    []byte
	buffer  *[]byte
	release func()
}

// chunkOrder is the reorder buffer between concurrent fetchers and the single
// sequential writer. It holds at most the chunks whose slots are taken.
type chunkOrder struct {
	mu      sync.Mutex
	count   int64
	next    int64
	ready   map[int64]*fetchedChunk
	arrived chan struct{}
	closed  bool
}

func newChunkOrder(count int64) *chunkOrder {
	return &chunkOrder{count: count, ready: map[int64]*fetchedChunk{}, arrived: make(chan struct{}, 1)}
}

// deliver hands a fetched chunk to the writer. After close, the chunk is
// released at once.
func (o *chunkOrder) deliver(index int64, chunk *fetchedChunk) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		chunk.release()
		return
	}
	o.ready[index] = chunk
	o.mu.Unlock()
	select {
	case o.arrived <- struct{}{}:
	default:
	}
}

// write appends chunks in index order as each next one arrives, releasing
// its slot and memory once it is written.
func (o *chunkOrder) write(ctx context.Context, w io.Writer, buffers *sync.Pool) error {
	for {
		o.mu.Lock()
		if o.next == o.count {
			o.mu.Unlock()
			return nil
		}
		chunk := o.ready[o.next]
		if chunk != nil {
			delete(o.ready, o.next)
			o.next++
		}
		o.mu.Unlock()
		if chunk == nil {
			select {
			case <-o.arrived:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			chunk.release()
			return err
		}
		_, err := w.Write(chunk.data)
		buffers.Put(chunk.buffer)
		chunk.release()
		if err != nil {
			return err
		}
	}
}

// close releases chunks that were fetched but never written.
func (o *chunkOrder) close(buffers *sync.Pool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	for index, chunk := range o.ready {
		delete(o.ready, index)
		buffers.Put(chunk.buffer)
		chunk.release()
	}
}
