package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Inventory discovery represents an absent key with only its key populated.
// An existing empty object has an identity and must never take this path.
func SourceIdentityUnknown(object transferstore.Object) bool {
	key := object.Key
	object.Key = ""
	return key != "" && reflect.DeepEqual(object, transferstore.Object{})
}

func validResolution(item Item, source transferstore.Object) bool {
	return item.Legacy == nil && SourceIdentityUnknown(item.Source) &&
		source.Key == item.Source.Key && source.Size >= 0 &&
		(source.ETag != "" || source.VersionID != "" || !source.ModifiedAt.IsZero())
}

// EffectiveSource keeps the immutable assignment separate from an identity that
// could only be obtained after a missing inventory object was supplied.
func EffectiveSource(item Item, result ItemResult) transferstore.Object {
	if result.ResolvedSource != nil {
		return *result.ResolvedSource
	}
	return item.Source
}

// validRefresh reports whether source can be the current version of an
// object discovered with a known identity, read again on a retry after the
// object changed.
func validRefresh(item Item, source transferstore.Object) bool {
	return item.Legacy == nil && source.Key == item.Source.Key && source.Size >= 0 &&
		(source.ETag != "" || source.VersionID != "" || !source.ModifiedAt.IsZero())
}

// ValidateResolvedSources checks Agent observations against the Master's inventory
// scope and previously observed pins. It also rejects removal of a durable pin.
// A pin may replace an object's discovered identity, or change, only on a
// result whose reason says the source was read again (see PendingResult).
func (r Record) ValidateResolvedSources(inventory bool, previous *Record) error {
	old := map[string]*transferstore.Object{}
	if previous != nil {
		for _, result := range previous.Results {
			if result.ResolvedSource != nil {
				old[result.ID] = result.ResolvedSource
			}
		}
	}
	items := map[string]Item{}
	for _, item := range r.Job.Items {
		items[item.ID] = item
	}
	seen := map[string]bool{}
	var total int64
	for _, result := range r.Results {
		item, ok := items[result.ID]
		refreshed := refreshReason(result.Reconciliation)
		if result.ResolvedSource != nil {
			resolution := inventory && r.Job.Source.Kind == "S3" && validResolution(item, *result.ResolvedSource)
			refresh := refreshed && validRefresh(item, *result.ResolvedSource)
			if !ok || seen[result.ID] || !resolution && !refresh {
				return errors.New("invalid resolved inventory source")
			}
		}
		if prior := old[result.ID]; prior != nil && !reflect.DeepEqual(prior, result.ResolvedSource) && (!refreshed || result.ResolvedSource == nil) {
			return errors.New("resolved inventory source changed")
		}
		delete(old, result.ID)
		seen[result.ID] = true
		if ok {
			size := EffectiveSource(item, result).Size
			if size < 0 || total > math.MaxInt64-size {
				return errors.New("resolved source size overflow")
			}
			total += size
		}
	}
	if len(old) != 0 {
		return errors.New("resolved inventory source is missing")
	}
	return nil
}

func (r *Repository) pinSource(ctx context.Context, child string, generation, attempt uint64, item Item, source transferstore.Object) (transferstore.Object, error) {
	var pinned transferstore.Object
	_, err := r.mutate(ctx, child, func(tx *state.Tx, record *Record) error {
		if err := checkAttempt(tx, *record, generation, attempt); err != nil {
			return err
		}
		if !record.InventorySource || record.Job.Source.Kind != "S3" || !validResolution(item, source) {
			return errors.New("source identity cannot be resolved")
		}
		for index, assigned := range record.Job.Items {
			if assigned.ID != item.ID {
				continue
			}
			if !reflect.DeepEqual(assigned, item) {
				return ErrConflict
			}
			result := &record.Results[index]
			if result.ResolvedSource != nil {
				pinned = *result.ResolvedSource
				return nil
			}
			if source.Size > math.MaxInt64-record.Counters.TotalBytes {
				return errors.New("resolved source size overflow")
			}
			if err := state.RequireSchema(tx, "agent", state.SchemaResolvedSources); err != nil {
				return err
			}
			result.ResolvedSource = &source
			pinned = source
			recount(record)
			return nil
		}
		return errors.New("unknown source item")
	})
	return pinned, err
}

// refreshSource pins the current version of a source that changed after
// discovery, before it is read. The object must still be pending with the
// request to read its source again, which only a retry of that failure sets.
func (r *Repository) refreshSource(ctx context.Context, child string, generation, attempt uint64, item Item, source transferstore.Object) (transferstore.Object, error) {
	_, err := r.mutate(ctx, child, func(tx *state.Tx, record *Record) error {
		if err := checkAttempt(tx, *record, generation, attempt); err != nil {
			return err
		}
		if !validRefresh(item, source) {
			return errors.New("source identity cannot be refreshed")
		}
		for index, assigned := range record.Job.Items {
			if assigned.ID != item.ID {
				continue
			}
			if !reflect.DeepEqual(assigned, item) {
				return ErrConflict
			}
			result := &record.Results[index]
			if result.Status != ItemPending || result.Reconciliation != ReasonSourceChanged {
				return errors.New("source refresh is no longer requested")
			}
			previous := EffectiveSource(item, *result).Size
			if source.Size > previous && source.Size-previous > math.MaxInt64-record.Counters.TotalBytes {
				return errors.New("resolved source size overflow")
			}
			if err := state.RequireSchema(tx, "agent", state.SchemaResolvedSources); err != nil {
				return err
			}
			result.ResolvedSource = &source
			recount(record)
			return nil
		}
		return errors.New("unknown source item")
	})
	return source, err
}

func (e *Engine) copyResolvedItem(ctx context.Context, record Record, item Item, source, target transferstore.Store, progress ...*attemptProgress) ItemResult {
	var pinned *transferstore.Object
	refresh, refreshed := false, false
	for _, result := range record.Results {
		if result.ID == item.ID {
			pinned = result.ResolvedSource
			refresh = result.Status == ItemPending && result.Reconciliation == ReasonSourceChanged
			refreshed = pinned != nil && refreshReason(result.Reconciliation)
			break
		}
	}
	if refresh && item.Legacy == nil && (pinned != nil || !SourceIdentityUnknown(item.Source)) {
		// A retry of an object whose source changed after discovery reads
		// the source's current version, which is pinned durably first.
		actual, err := source.Stat(ctx, item.Source.Key)
		if errors.Is(err, transferstore.ErrNotFound) {
			result := sourceDeleted(item)
			result.ResolvedSource = pinned
			return result
		}
		if err == nil {
			actual, err = e.repo.refreshSource(ctx, record.Job.ChildID, record.Job.Generation, record.Attempt, item, actual)
		}
		if err != nil {
			// The next retry tries to read the current version again.
			return ItemResult{ID: item.ID, Status: ItemFailed, ResolvedSource: pinned, Reconciliation: ReasonSourceChanged, Error: errorText(fmt.Errorf("read current source %q: %w", item.Source.Key, err))}
		}
		pinned, refreshed = &actual, true
	}
	if pinned == nil && record.InventorySource && record.Job.Source.Kind == "S3" && item.Legacy == nil && SourceIdentityUnknown(item.Source) {
		actual, err := source.Stat(ctx, item.Source.Key)
		if err == nil {
			actual, err = e.repo.pinSource(ctx, record.Job.ChildID, record.Job.Generation, record.Attempt, item, actual)
		}
		if err != nil {
			return ItemResult{ID: item.ID, Status: ItemFailed, Error: errorText(fmt.Errorf("resolve source %q: %w", item.Source.Key, err))}
		}
		pinned = &actual
	}
	if pinned != nil {
		item.Source = *pinned
	}
	var object *objectProgress
	if len(progress) > 0 && progress[0] != nil {
		object = progress[0].begin(item)
	}
	result := e.copyItem(ctx, record.Job, item, source, target, object)
	if object != nil {
		progress[0].finish(object, result.Status == ItemSucceeded)
	}
	result.ResolvedSource = pinned
	if refreshed && result.Reconciliation == "" {
		result.Reconciliation = ReasonSourceRefreshed
	}
	return result
}
