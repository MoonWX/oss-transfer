package execution

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/state"
)

type QueueReleaseRequest struct {
	ParentID   int64  `json:"parentId"`
	ChildID    string `json:"childId"`
	Generation uint64 `json:"generation"`
	Assignment uint64 `json:"assignment"`
	OwnerEpoch uint64 `json:"ownerEpoch"`
}

// A failover seed can be RECOVERABLE without ever having run on any node.
// Only that empty-attempt case is equivalent to an ordinary waiting job.
func (r Record) QueuedForHandoff() bool {
	if (r.Status != Waiting && r.Status != Recoverable) || r.Attempt != 0 || r.StartedAt != nil {
		return false
	}
	c := r.Counters
	if c.Succeeded != 0 || c.Skipped != 0 || c.Failed != 0 || c.SucceededBytes != 0 || c.SkippedBytes != 0 || c.FailedBytes != 0 {
		return false
	}
	for _, result := range r.Results {
		if result.Status != ItemPending || result.TargetCommitted {
			return false
		}
	}
	return true
}

// ReleaseQueued wins or loses against Start under the same admission lock and
// database transaction. Only work which has never started can be transferred.
// The record itself retains the revocation across lost replies and restarts.
func (e *Engine) ReleaseQueued(ctx context.Context, req QueueReleaseRequest) (released bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false, ErrClosed
	}
	if e.active[req.ChildID] != nil {
		return false, nil
	}
	err = e.repo.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := load(tx, req.ChildID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Job.ParentID != req.ParentID || row.Job.Assignment != req.Assignment || row.Job.OwnerEpoch != req.OwnerEpoch {
			return ErrAssignment
		}
		if row.Job.Generation > req.Generation {
			return nil
		}
		if row.Job.Generation != req.Generation {
			return ErrGeneration
		}
		if row.ReleasedAssignment != nil && *row.ReleasedAssignment == req.Assignment {
			released = true
			return nil
		}
		if !row.QueuedForHandoff() {
			return nil
		}
		if err := checkParent(tx, req.ParentID, req.Generation); err != nil {
			if errors.Is(err, ErrGeneration) || errors.Is(err, ErrStopped) || errors.Is(err, ErrDeleted) {
				return nil
			}
			return err
		}
		if _, err := e.jobContext(row.Job); err != nil {
			return err
		}
		if err := state.RequireSchema(tx, "agent", state.SchemaTaskHandoff); err != nil {
			return err
		}
		next := row
		next.Status, next.ReleasedAssignment = Retired, &req.Assignment
		if err := e.repo.put(tx, &row, &next); err != nil {
			return err
		}
		released = true
		return nil
	})
	return
}
