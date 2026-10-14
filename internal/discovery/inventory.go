package discovery

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func (s *Scanner) scanInventory(ctx context.Context, attempt State, source transferstore.Store, emit func(transferstore.Object, bool) error) error {
	repo := inventory.New(s.store)
	header, err := repo.Get(ctx, attempt.Spec.InventoryID)
	if err != nil {
		return err
	}
	if header.ParentID != attempt.Spec.ParentID {
		return inventory.ErrConflict
	}
	if !inventory.Ready(header) {
		return inventory.ErrUnsealed
	}
	after := ""
	for {
		keys, next, err := repo.Keys(ctx, header.ID, after, inventory.MaxBatchKeys)
		if err != nil {
			return err
		}
		for _, key := range keys {
			object, err := source.Stat(ctx, key)
			missing := errors.Is(err, transferstore.ErrNotFound)
			if missing {
				object = transferstore.Object{Key: key}
			} else if err != nil {
				return err
			}
			if err = emit(object, missing); err != nil {
				return err
			}
		}
		if len(keys) < inventory.MaxBatchKeys {
			return nil
		}
		after = next
	}
}
