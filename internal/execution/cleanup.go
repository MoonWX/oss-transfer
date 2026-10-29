package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// DeletionProgress reports only committed cleanup, not an estimate of work in
// an uncommitted transaction. The compact parent fence preserves this on restart.
type DeletionProgress struct {
	Done           bool   `json:"done"`
	RecordsDeleted int64  `json:"recordsDeleted"`
	Error          string `json:"error,omitempty"`
}

// RequestDeleteParent fences new work and cancels active attempts without waiting
// for source I/O or scanning old ledgers. The Engine owns the cleanup lifetime.
func (e *Engine) RequestDeleteParent(ctx context.Context, id int64, generation uint64) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if err := e.repo.fenceParent(ctx, id, generation, true); err != nil {
		e.mu.Unlock()
		return err
	}
	for _, attempt := range e.active {
		if attempt.parentID == id {
			attempt.cancel()
		}
	}
	e.mu.Unlock()
	select {
	case e.cleanupWake <- struct{}{}:
	default:
	}
	return nil
}

func (e *Engine) DeletionProgress(ctx context.Context, id int64, generation uint64) (DeletionProgress, error) {
	p, err := e.repo.Parent(ctx, id)
	if err != nil {
		return DeletionProgress{}, err
	}
	if p.Generation != generation {
		return DeletionProgress{}, ErrGeneration
	}
	if !p.Deleted {
		return DeletionProgress{}, errors.New("parent cleanup was not requested")
	}
	return DeletionProgress{Done: p.CleanupDone, RecordsDeleted: p.CleanupDeleted, Error: p.CleanupError}, nil
}

func (r *Repository) nextPurge(ctx context.Context, after string) (key string, id int64, generation uint64, err error) {
	err = r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Scan(purgesBucket, "", after, 1, func(k string, raw json.RawMessage) error {
			var err error
			id, err = strconv.ParseInt(k, 10, 64)
			if err != nil || id < 1 {
				return errors.New("invalid execution purge parent")
			}
			if err = json.Unmarshal(raw, &generation); err != nil {
				return err
			}
			key = k
			return nil
		})
	})
	return
}

func (r *Repository) cleanupProblem(id int64, generation uint64, problem error) {
	if problem == nil || errors.Is(problem, context.Canceled) {
		return
	}
	_ = r.db.Update(func(tx *state.Tx) error {
		p, found, err := parent(tx, id)
		if err != nil || !found || p.Generation != generation || !p.Deleted || p.CleanupDone {
			return err
		}
		p.CleanupError = errorText(problem)
		return tx.Put(parentsBucket, parentKey(id), p)
	})
}

// One worker per Engine makes disk demand independent of the number of deleted
// tasks. It rotates through the durable queue and yields between bounded pages.
func (e *Engine) cleanupLoop() {
	defer close(e.cleanupDone)
	var after string
	for e.ctx.Err() == nil {
		key, id, generation, err := e.repo.nextPurge(e.ctx, after)
		if err == nil && key == "" && after != "" {
			after = ""
			continue
		}
		if err == nil && key != "" {
			after = key
			active := false
			e.mu.Lock()
			for _, attempt := range e.active {
				if attempt.parentID == id {
					active = true
					break
				}
			}
			e.mu.Unlock()
			if !active {
				_, err = e.repo.purgeDeletedPage(e.ctx, id, generation)
				e.repo.cleanupProblem(id, generation, err)
			}
		}
		delay := 50 * time.Millisecond
		var wake <-chan struct{}
		if err != nil || key == "" {
			delay = time.Second
			wake = e.cleanupWake
		}
		timer := time.NewTimer(delay)
		select {
		case <-e.ctx.Done():
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}
