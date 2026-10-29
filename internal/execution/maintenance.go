package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const maintenancePageSize = 128

type runningIndexState struct {
	After    string `json:"after,omitempty"`
	Complete bool   `json:"complete"`
}

// Upgrade old databases once, in bounded transactions. The cursor commits with
// each page, so interruption resumes the build instead of rescanning history.
// Production databases are claimed with schema 4 before repositories open;
// older binaries cannot subsequently write jobs without maintaining this index.
func (r *Repository) ensureRunningIndex(ctx context.Context) error {
	var index runningIndexState
	if err := r.db.View(func(tx *state.Tx) error {
		_, err := tx.Get(metaBucket, "running-index-v1", &index)
		return err
	}); err != nil || index.Complete {
		return err
	}
	for !index.Complete {
		if err := r.db.Update(func(tx *state.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			count := 0
			if err := tx.Scan(jobsBucket, "", index.After, maintenancePageSize, func(key string, raw json.RawMessage) error {
				var record Record
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				if record.Status == Running {
					if err := tx.Put(runningBucket, key, record.Job.ChildID); err != nil {
						return err
					}
				}
				if record.Status == Deleted {
					fence, found, err := parent(tx, record.Job.ParentID)
					if err != nil {
						return err
					}
					if found && fence.Deleted {
						if err := tx.Put(purgesBucket, parentKey(fence.ParentID), fence.Generation); err != nil {
							return err
						}
					}
				}
				index.After, count = key, count+1
				return nil
			}); err != nil {
				return err
			}
			index.Complete = count < maintenancePageSize
			return tx.Put(metaBucket, "running-index-v1", index)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) running(ctx context.Context, after string, limit int) ([]Record, error) {
	var records []Record
	afterKey := ""
	if after != "" {
		var err error
		afterKey, err = jobKey(after)
		if err != nil {
			return nil, err
		}
	}
	err := r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Scan(runningBucket, "", afterKey, limit, func(_ string, raw json.RawMessage) error {
			var child string
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			record, err := load(tx, child)
			if err != nil {
				return err
			}
			if record.Status != Running {
				return errors.New("inconsistent execution running index")
			}
			records = append(records, record)
			return nil
		})
	})
	return records, err
}

// Only call after every attempt has exited, or during offline recovery. A page
// commits its cursor, counters and deletions together, so restarts never rescan
// already removed data. Status-only decoding avoids allocating object ledgers.
func (r *Repository) purgeDeletedPage(ctx context.Context, id int64, generation uint64) (done bool, err error) {
	err = r.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fence, found, err := parent(tx, id)
		if err != nil {
			return err
		}
		if !found || fence.Generation != generation {
			return ErrGeneration
		}
		if !fence.Deleted {
			return errors.New("cannot purge an undeleted parent")
		}
		if fence.CleanupDone {
			done = true
			return tx.Delete(purgesBucket, parentKey(id))
		}
		budget := state.DefaultCleanupBudget()
		started := time.Now()
		var rows []string
		var storedBytes int64
		full := false
		err = tx.ScanKeys(jobsBucket, fmt.Sprintf("%020d/", id), fence.CleanupAfter, budget.MaxRecords+1, func(key string, size int64) error {
			if len(rows) > 0 && (len(rows) >= budget.MaxRecords || storedBytes+int64(size) > budget.MaxBytes || time.Since(started) >= budget.MaxDuration) {
				full = true
				return errPurgePageFull
			}
			rows = append(rows, key)
			storedBytes += int64(size)
			return nil
		})
		if err != nil && !errors.Is(err, errPurgePageFull) {
			return err
		}
		var u usage
		if _, err := tx.Get(metaBucket, "usage", &u); err != nil {
			return err
		}
		if u.Pending < 0 {
			return errors.New("invalid pending job counter during purge")
		}
		var removedPending, removed int64
		for _, row := range rows {
			if removed > 0 && time.Since(started) >= budget.MaxDuration {
				full = true
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// The admission counter is maintained atomically with every job.
			// Once no unfinished jobs remain, all history can be removed by key
			// without inflating any object ledger. Busy Agents still inspect the
			// small status field to debit only this parent's pending jobs.
			if u.Pending > removedPending {
				var status struct {
					Status Status `json:"status"`
				}
				found, err := tx.Get(jobsBucket, row, &status)
				if err != nil {
					return err
				}
				if !found {
					return ErrNotFound
				}
				if !terminal(status.Status) {
					removedPending++
				}
			}
			for _, bucket := range []string{jobsBucket, readyBucket, runningBucket} {
				if err := tx.Delete(bucket, row); err != nil {
					return err
				}
			}
			fence.CleanupAfter = row
			removed++
		}
		if removedPending > 0 {
			if u.Pending < removedPending {
				return errors.New("invalid pending job counter during purge")
			}
			u.Pending -= removedPending
			if err := tx.Put(metaBucket, "usage", u); err != nil {
				return err
			}
		}
		fence.CleanupDeleted += removed
		fence.CleanupError = ""
		done = !full && removed == int64(len(rows))
		if done {
			fence.CleanupDone = true
			fence.CleanupAfter = ""
			if err := tx.Delete(purgesBucket, parentKey(id)); err != nil {
				return err
			}
		}
		return tx.Put(parentsBucket, parentKey(id), fence)
	})
	return
}

var errPurgePageFull = errors.New("execution cleanup page full")

func (r *Repository) purgeDeleted(ctx context.Context, id int64, generation uint64) error {
	for {
		done, err := r.purgeDeletedPage(ctx, id, generation)
		if err != nil || done {
			return err
		}
	}
}

func (r *Repository) purgePending(ctx context.Context) error {
	for {
		type pending struct {
			id         int64
			generation uint64
		}
		var page []pending
		if err := r.db.View(func(tx *state.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return tx.Scan(purgesBucket, "", "", maintenancePageSize, func(key string, raw json.RawMessage) error {
				id, err := strconv.ParseInt(key, 10, 64)
				if err != nil || id < 1 {
					return errors.New("invalid execution purge parent")
				}
				var generation uint64
				if err := json.Unmarshal(raw, &generation); err != nil {
					return err
				}
				page = append(page, pending{id, generation})
				return nil
			})
		}); err != nil {
			return err
		}
		for _, pending := range page {
			if err := r.purgeDeleted(ctx, pending.id, pending.generation); err != nil {
				return err
			}
		}
		if len(page) < maintenancePageSize {
			return nil
		}
	}
}
