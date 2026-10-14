package ratelimit

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// Decorator is directly assignable to execution.Options.DecorateReader. Install
// a parent allocation before accepting its work, including an explicit zero for
// unlimited bandwidth. The job generation must match the installed allocation.
func (l *Limiter) Decorator(ctx context.Context, job execution.Job, item execution.Item, input io.ReadCloser) (io.ReadCloser, error) {
	return l.Wrap(ctx, job.ParentID, job.Generation, input, item.Source.Size)
}

func (l *Limiter) Wrap(ctx context.Context, parentID int64, generation uint64, input io.ReadCloser, size int64) (io.ReadCloser, error) {
	if input == nil || size < 0 {
		return nil, errors.New("input and exact nonnegative size are required")
	}
	l.mu.Lock()
	p, err := l.checkParent(parentID, generation)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	parentCtx := p.ctx
	l.mu.Unlock()
	readCtx, cancel := context.WithCancelCause(ctx)
	r := &reader{limiter: l, input: input, ctx: readCtx, cancel: cancel, parentID: parentID, generation: generation, remaining: size}
	r.stopParent = context.AfterFunc(parentCtx, func() { cancel(context.Cause(parentCtx)) })
	r.stopInput = context.AfterFunc(readCtx, func() { r.closeInput() })
	return r, nil
}

type reader struct {
	limiter               *Limiter
	input                 io.ReadCloser
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	parentID              int64
	generation            uint64
	remaining             int64
	readMu                sync.Mutex
	closeOnce             sync.Once
	closeErr              error
	stopParent, stopInput func() bool
}

func (r *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	wanted := min(len(p), r.limiter.opts.ChunkBytes)
	if r.remaining > 0 {
		wanted = int(min(int64(wanted), r.remaining))
	} else {
		wanted = 1
	} // bounded EOF check; extra source data remains detectable
	ticket, err := r.limiter.reserve(r.ctx, r.parentID, r.generation, wanted)
	if err != nil {
		return 0, err
	}
	if err := r.ctx.Err(); err != nil {
		r.limiter.refund(ticket, ticket.size)
		return 0, context.Cause(r.ctx)
	}
	n, err := r.input.Read(p[:ticket.size])
	if n < 0 || n > ticket.size {
		return 0, errors.New("source returned an invalid read count")
	}
	r.limiter.refund(ticket, ticket.size-n)
	r.remaining = max(0, r.remaining-int64(n))
	if r.ctx.Err() != nil {
		return n, context.Cause(r.ctx)
	}
	return n, err
}
func (r *reader) closeInput() { r.closeOnce.Do(func() { r.closeErr = r.input.Close() }) }
func (r *reader) Close() error {
	r.stopParent()
	r.cancel(context.Canceled)
	r.stopInput()
	r.closeInput()
	return r.closeErr
}
