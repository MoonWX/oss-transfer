package execution

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// RetryFailed reopens an ended child so that only its failed objects run
// again, in the current generation and without touching the parent's other
// children. The child returns to the normal ready queue: the scheduler starts
// it when an execution slot is free, under the same lease and shared-mount
// checks as any other job. attempt is the ended attempt the caller observed;
// a replayed request or one based on an older observation changes nothing
// and reports reopened=false with the current record.
func (e *Engine) RetryFailed(ctx context.Context, child string, generation, attempt uint64) (record Record, reopened bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, false, ErrClosed
	}
	current, err := e.repo.Get(ctx, child)
	if err != nil {
		return Record{}, false, err
	}
	admission := current.Job
	admission.Generation = generation
	if _, err := e.jobContext(admission); err != nil {
		return Record{}, false, err
	}
	return e.repo.retryFailed(ctx, child, generation, attempt)
}

// retryFailed returns the failed results of an ended child to pending.
// Succeeded and skipped results keep their credit; like every attempt, the
// next one revalidates them against the target before it runs. The pending
// job counter and the ready index change in the same transaction, so a full
// Agent queue refuses the retry (ErrCapacity) instead of exceeding its bound.
func (r *Repository) retryFailed(ctx context.Context, child string, generation, attempt uint64) (Record, bool, error) {
	reopened := false
	record, err := r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if j.ReleasedAssignment != nil {
			return ErrAssignment
		}
		if err := checkParent(tx, j.Job.ParentID, generation); err != nil {
			return err
		}
		if j.Job.Generation != generation {
			return ErrGeneration
		}
		if j.Attempt != attempt || (j.Status != Failed && j.Status != PartialFailed) || j.Counters.Failed == 0 {
			return nil
		}
		for i := range j.Results {
			if j.Results[i].Status == ItemFailed {
				j.Results[i] = PendingResult(j.Results[i])
			}
		}
		recount(j)
		j.Attempt++
		j.Status = Waiting
		j.Error = ""
		reopened = true
		return nil
	})
	if err != nil {
		return Record{}, false, err
	}
	return record, reopened, nil
}
