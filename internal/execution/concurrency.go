package execution

import (
	"context"
	"errors"
)

const (
	DefaultChildConcurrency  = 8
	DefaultObjectConcurrency = 4
)

// SetConcurrency updates admission for both existing and future children.
// Transfers already admitted finish normally when either limit is lowered.
func (e *Engine) SetConcurrency(children, objects int) error {
	if children < 1 || children > 1024 || objects < 1 || objects > 128 {
		return errors.New("invalid execution bounds")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	e.opts.MaxConcurrentJobs, e.opts.WorkersPerJob = children, objects
	e.wakeConcurrency()
	for _, attempt := range e.active {
		attempt.wakeObjects()
	}
	return nil
}

func (e *Engine) ObjectConcurrency() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opts.WorkersPerJob
}

func (e *Engine) wakeConcurrency() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *Engine) acquireObject(ctx context.Context, attempt *activeAttempt) error {
	for {
		e.mu.Lock()
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return err
		}
		if e.closed {
			e.mu.Unlock()
			return ErrClosed
		}
		if attempt.objects < e.opts.WorkersPerJob {
			attempt.objects++
			e.mu.Unlock()
			return nil
		}
		changed := attempt.objectsChanged
		e.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *Engine) releaseObject(attempt *activeAttempt) {
	e.mu.Lock()
	attempt.objects--
	attempt.wakeObjects()
	e.mu.Unlock()
}

func (attempt *activeAttempt) wakeObjects() {
	close(attempt.objectsChanged)
	attempt.objectsChanged = make(chan struct{})
}
