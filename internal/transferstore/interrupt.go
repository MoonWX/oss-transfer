package transferstore

import (
	"context"
	"errors"
)

// Causes an Agent cancels transfer work with (context.Cause). They tell
// whether the same Agent may continue the task later.
var (
	// ErrInterrupted: a stop, a shutdown, or a lease that expired or was
	// re-armed. The same Agent may continue the task later.
	ErrInterrupted = errors.New("transfer interrupted")
	// ErrMoved: the task's work moves to another Agent (retire, rejoin).
	ErrMoved = errors.New("transfer moved to another Agent")
	// ErrTaskDeleted: the task was deleted permanently.
	ErrTaskDeleted = errors.New("task permanently deleted")
)

// Interrupted reports whether ctx was cancelled by an interruption.
func Interrupted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrInterrupted)
}
