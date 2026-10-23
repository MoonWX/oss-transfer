package discovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const inventoryMetadataConcurrency = 16

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
		parallelism := 1
		if attempt.Spec.Source.Kind == "S3" {
			parallelism = inventoryMetadataConcurrency
		}
		if err := scanInventoryPage(ctx, source, keys, parallelism, emit); err != nil {
			return err
		}
		if len(keys) < inventory.MaxBatchKeys {
			return nil
		}
		after = next
	}
}

// HEAD requests may overlap, but delivery preserves inventory order and batch
// boundaries. A single page bounds both queued metadata and look-ahead. The
// source's request limiter still applies to every HEAD. Local manifests remain
// serial to avoid turning directory traversal into extra random disk pressure.
func scanInventoryPage(ctx context.Context, source transferstore.Store, keys []string, parallelism int, emit func(transferstore.Object, bool) error) error {
	if len(keys) == 0 {
		return ctx.Err()
	}
	emitResult := func(key string, object transferstore.Object, err error) error {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		missing := errors.Is(err, transferstore.ErrNotFound)
		if missing {
			object = transferstore.Object{Key: key}
		} else if err != nil {
			return err
		}
		return emit(object, missing)
	}
	if parallelism <= 1 {
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			object, err := source.Stat(ctx, key)
			if err := emitResult(key, object, err); err != nil {
				return err
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	type result struct {
		object transferstore.Object
		err    error
	}
	results := make([]chan result, len(keys))
	for i := range results {
		results[i] = make(chan result, 1)
	}
	var next atomic.Int64
	for range min(max(parallelism, 1), len(keys)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				index := int(next.Add(1) - 1)
				if index >= len(keys) {
					return
				}
				object, err := source.Stat(ctx, keys[index])
				results[index] <- result{object: object, err: err}
			}
		}()
	}
	for i, ready := range results {
		var current result
		select {
		case <-ctx.Done():
			return ctx.Err()
		case current = <-ready:
		}
		if err := emitResult(keys[i], current.object, current.err); err != nil {
			return err
		}
	}
	return nil
}
