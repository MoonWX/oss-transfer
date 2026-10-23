package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

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

// Only call after every attempt has exited, or during startup before execution
// begins. The small parent tombstone remains durable to reject late commands.
func (r *Repository) purgeDeleted(ctx context.Context, id int64, generation uint64) error {
	for {
		done := false
		err := r.db.Update(func(tx *state.Tx) error {
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
			type row struct {
				key     string
				pending bool
			}
			var rows []row
			if err := tx.Scan(jobsBucket, fmt.Sprintf("%020d/", id), "", maintenancePageSize, func(key string, raw json.RawMessage) error {
				var record Record
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				rows = append(rows, row{key, !terminal(record.Status)})
				return nil
			}); err != nil {
				return err
			}
			var removedPending int64
			for _, row := range rows {
				for _, bucket := range []string{jobsBucket, readyBucket, runningBucket} {
					if err := tx.Delete(bucket, row.key); err != nil {
						return err
					}
				}
				if row.pending {
					removedPending++
				}
			}
			if removedPending > 0 {
				var u usage
				if _, err := tx.Get(metaBucket, "usage", &u); err != nil {
					return err
				}
				if u.Pending < removedPending {
					return errors.New("invalid pending job counter during purge")
				}
				u.Pending -= removedPending
				if err := tx.Put(metaBucket, "usage", u); err != nil {
					return err
				}
			}
			done = len(rows) < maintenancePageSize
			if done {
				return tx.Delete(purgesBucket, parentKey(id))
			}
			return nil
		})
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
