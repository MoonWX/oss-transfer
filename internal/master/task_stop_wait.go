package master

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// A control RPC can expire while an Agent is persisting its stop fence or
// draining an in-flight request. Keep retrying in STOPPING; a missing reply
// cannot prove either a failed transfer or a completed stop. Only a short
// series of timeouts gets this treatment, so an unresponsive Agent stays visible.
const stopAcknowledgementGrace = 30 * time.Second

type stopWait struct {
	generation uint64
	since      time.Time
}

type stopAwaitingAcknowledgement struct{ cause error }

func (e *stopAwaitingAcknowledgement) Error() string { return e.cause.Error() }
func (e *stopAwaitingAcknowledgement) Unwrap() error { return e.cause }

func isStopAwaitingAcknowledgement(err error) bool {
	var pending *stopAwaitingAcknowledgement
	return errors.As(err, &pending)
}

// Check every joined error. errors.Is alone would also match a timeout joined
// with a disk, generation or permission error and accidentally hide that fault.
func onlyStopTimeouts(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyStopTimeouts(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyStopTimeouts(wrapped.Unwrap())
	}
	return errors.Is(err, context.DeadlineExceeded) || agentStatus(err, 408)
}

func (t *taskRuntime) stopControlProblem(p task.Parent, err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		delete(t.stopWaits, p.ID)
		return nil
	}
	if p.Status != "STOPPING" || !onlyStopTimeouts(err) {
		return err
	}
	now := t.server.options.Now()
	wait, found := t.stopWaits[p.ID]
	if !found || wait.generation != p.Generation {
		wait = stopWait{generation: p.Generation, since: now}
		if t.stopWaits == nil {
			t.stopWaits = make(map[int64]stopWait)
		}
		t.stopWaits[p.ID] = wait
	}
	if now.Sub(wait.since) >= stopAcknowledgementGrace {
		return err
	}
	return &stopAwaitingAcknowledgement{cause: err}
}
