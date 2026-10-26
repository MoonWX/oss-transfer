package execution

import (
	"context"
	"errors"
	"reflect"
)

var ErrAssignment = errors.New("stale child assignment")

// AcceptSeed admits an observed ledger on its new owner. A duplicate admission
// never replaces newer local progress. The seeded job must pass normal target
// revalidation before it can be marked complete.
func (e *Engine) AcceptSeed(ctx context.Context, job Job, seed *Record, inventorySource ...bool) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, ErrClosed
	}
	if e.opts.ParentContext != nil {
		if _, err := e.opts.ParentContext(job.ParentID, job.Generation); err != nil {
			return Record{}, err
		}
	}
	if seed != nil && e.active[job.ChildID] != nil {
		current, err := e.repo.Get(ctx, job.ChildID)
		if err != nil {
			return Record{}, err
		}
		if current.Job.Assignment != job.Assignment {
			return Record{}, ErrRunning
		}
	}
	return e.repo.accept(ctx, job, seed, inventorySource...)
}

func importedRecord(job Job, seed *Record, fingerprint string, inventorySource bool) (Record, error) {
	if seed == nil || seed.Job.ChildID != job.ChildID || seed.Job.ParentID != job.ParentID || seed.Job.Number != job.Number || seed.Job.Generation > job.Generation || len(seed.Job.Items) != len(job.Items) {
		return Record{}, ErrConflict
	}
	oldItems := make(map[string]Item, len(seed.Job.Items))
	for _, item := range seed.Job.Items {
		if _, exists := oldItems[item.ID]; exists {
			return Record{}, ErrConflict
		}
		oldItems[item.ID] = item
	}
	results := make(map[string]ItemResult, len(seed.Results))
	for _, result := range seed.Results {
		if _, exists := oldItems[result.ID]; !exists {
			return Record{}, ErrConflict
		}
		if _, exists := results[result.ID]; exists {
			return Record{}, ErrConflict
		}
		results[result.ID] = result
	}
	result := Record{Job: job, Status: Recoverable, Attempt: seed.Attempt, Fingerprint: fingerprint, InventorySource: inventorySource, CreatedAt: seed.CreatedAt, StartedAt: seed.StartedAt, Results: make([]ItemResult, len(job.Items))}
	timing := seed.ExecutionTime()
	timing.Transition(false, seed.UpdatedAt)
	result.ActiveTime = &timing
	for i, item := range job.Items {
		old, ok := oldItems[item.ID]
		if !ok || !reflect.DeepEqual(old.Source, item.Source) || old.DestinationKey != item.DestinationKey || !reflect.DeepEqual(old.Legacy, item.Legacy) {
			return Record{}, ErrConflict
		}
		credited, ok := results[item.ID]
		if !ok {
			credited = ItemResult{ID: item.ID, Status: ItemPending}
		}
		if credited.Status == ItemSucceeded || credited.Status == ItemSkipped {
			if credited.Target.Key != item.DestinationKey {
				return Record{}, ErrConflict
			}
			result.Results[i] = credited
		} else {
			result.Results[i] = ItemResult{ID: item.ID, Status: ItemPending, ResolvedSource: credited.ResolvedSource}
		}
	}
	recount(&result)
	return result, nil
}
