package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MoonWX/oss-transfer/internal/state"
)

type ownershipRetirement struct {
	Epoch uint64 `json:"epoch"`
	After string `json:"after,omitempty"`
	Done  bool   `json:"done"`
}

// RetireOwnership removes old owners from admission accounting without deleting
// their reports. Page cursor, queue indexes and counters commit together.
func (r *Repository) RetireOwnership(ctx context.Context, parentID int64, epoch uint64) error {
	for {
		done := false
		err := r.db.Update(func(tx *state.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var fence ownershipRetirement
			if _, err := tx.Get("execution-owner-retirements", parentKey(parentID), &fence); err != nil {
				return err
			}
			if fence.Epoch > epoch {
				return ErrAssignment
			}
			if fence.Epoch < epoch {
				fence = ownershipRetirement{Epoch: epoch}
			}
			if fence.Done {
				done = true
				return nil
			}
			var page []Record
			if err := tx.Scan(jobsBucket, fmt.Sprintf("%020d/", parentID), fence.After, 128, func(key string, raw json.RawMessage) error {
				var row Record
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				page = append(page, row)
				fence.After = key
				return nil
			}); err != nil {
				return err
			}
			for _, before := range page {
				if before.Job.OwnerEpoch >= epoch || terminal(before.Status) {
					continue
				}
				next := before
				next.Status = Retired
				if err := r.put(tx, &before, &next); err != nil {
					return err
				}
			}
			fence.Done = len(page) < 128
			done = fence.Done
			return tx.Put("execution-owner-retirements", parentKey(parentID), fence)
		})
		if err != nil || done {
			return err
		}
	}
}
