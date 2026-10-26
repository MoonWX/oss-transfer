package master

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/MoonWX/oss-transfer/internal/task"
)

const taskControlConcurrency = 4

type taskControlOutcome struct {
	parent   task.Parent
	admitted bool
	err      error
}

// Control requests share a small execution budget. Large selections do not
// create a goroutine per task, and simultaneous requests cannot each consume
// another full set of Agent command slots.
func (s *Server) runTaskControls(ctx context.Context, ids []int64, control func(context.Context, int64) (task.Parent, bool, error)) []taskControlOutcome {
	results := make([]taskControlOutcome, len(ids))
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(taskControlConcurrency, len(ids)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(ids) {
					return
				}
				results[index] = s.runTaskControl(ctx, ids[index], control)
			}
		}()
	}
	workers.Wait()
	return results
}

func (s *Server) runTaskControl(ctx context.Context, id int64, control func(context.Context, int64) (task.Parent, bool, error)) taskControlOutcome {
	if err := ctx.Err(); err != nil {
		return taskControlOutcome{err: err}
	}
	select {
	case s.taskControlSlots <- struct{}{}:
		defer func() { <-s.taskControlSlots }()
	case <-ctx.Done():
		return taskControlOutcome{err: ctx.Err()}
	}
	// A released slot and cancellation can become ready together. Check again
	// before starting a mutation, rather than letting select pick the slot.
	if err := ctx.Err(); err != nil {
		return taskControlOutcome{err: err}
	}
	p, admitted, err := control(ctx, id)
	return taskControlOutcome{parent: p, admitted: admitted, err: err}
}
