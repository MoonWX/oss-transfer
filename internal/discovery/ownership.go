package discovery

import (
	"context"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// AdoptCompletedOwner permits a returned node to serve its finished manifest
// under a fresh node lease. It never restarts enumeration or discards inventory.
// The caller must have revoked the old lease and quiesced all parent I/O.
func (s *Scanner) AdoptCompletedOwner(ctx context.Context, parentID int64, generation, epoch uint64, through int64, ownerEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.isActiveLocked(parentID) {
		return ErrRunning
	}
	return s.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := get(tx, parentID)
		if err != nil {
			return err
		}
		if current.Deleted {
			return ErrDeleted
		}
		if current.Generation != generation {
			return ErrGeneration
		}
		if current.Epoch != epoch || current.Spec.OwnerEpoch > ownerEpoch {
			return ErrEpoch
		}
		if !current.ScanComplete || current.LastBatch != through {
			return ErrConflict
		}
		if current.Spec.OwnerEpoch == ownerEpoch {
			return nil
		}
		current.Spec.OwnerEpoch = ownerEpoch
		current.UpdatedAt = time.Now().UTC()
		return tx.Put(states, parentKey(parentID), current)
	})
}
