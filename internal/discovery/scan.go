package discovery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type discoveredObject struct {
	Object      transferstore.Object `json:"object"`
	SeenAttempt string               `json:"seenAttempt"`
}

func objectKey(parentID int64, key string) string {
	return parentKey(parentID) + "/" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func sameObject(left, right transferstore.Object) bool {
	return left.Key == right.Key && left.Size == right.Size && left.ETag == right.ETag &&
		left.VersionID == right.VersionID && left.ModifiedAt.Equal(right.ModifiedAt) &&
		left.ContentType == right.ContentType && left.ContentEncoding == right.ContentEncoding &&
		left.ContentDisposition == right.ContentDisposition && left.CacheControl == right.CacheControl &&
		reflect.DeepEqual(left.Metadata, right.Metadata)
}

func copyObject(object transferstore.Object) transferstore.Object {
	if object.Metadata != nil {
		metadata := make(map[string]string, len(object.Metadata))
		for key, value := range object.Metadata {
			metadata[key] = value
		}
		object.Metadata = metadata
	}
	return object
}

func changed(key string) error { return fmt.Errorf("%w: %q", ErrSourceChanged, key) }

func (s *Scanner) commitBatch(ctx context.Context, attempt State, pending []transferstore.Object, missing ...map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.store.Update(func(tx *state.Tx) error {
		current, err := get(tx, attempt.Spec.ParentID)
		if err != nil {
			return err
		}
		if current.Generation != attempt.Generation || current.Attempt != attempt.Attempt {
			return ErrGeneration
		}
		if current.Status != Scanning {
			return ErrStopped
		}
		batch := Batch{ParentID: attempt.Spec.ParentID, Generation: current.Generation, Objects: make([]transferstore.Object, 0, len(pending))}
		for _, object := range pending {
			key := objectKey(attempt.Spec.ParentID, object.Key)
			var prior discoveredObject
			found, err := tx.Get(objects, key, &prior)
			if err != nil {
				return err
			}
			if found && !sameObject(prior.Object, object) {
				return changed(object.Key)
			}
			if prior.SeenAttempt == attempt.Attempt {
				continue
			}
			if !found {
				if current.TotalObjects == math.MaxInt64 || object.Size < 0 || object.Size > math.MaxInt64-current.TotalBytes {
					return ErrInvalid
				}
				candidate, err := s.legacyCandidate(tx, attempt.Spec, object, len(missing) > 0 && missing[0][object.Key])
				if err != nil {
					return err
				}
				if candidate != nil {
					if batch.Legacy == nil {
						batch.Legacy = map[string]execution.LegacyCandidate{}
					}
					batch.Legacy[object.Key] = *candidate
				}
				current.TotalObjects++
				current.TotalBytes += object.Size
				batch.Objects = append(batch.Objects, object)
				batch.Bytes += object.Size
			}
			current.SeenObjects++
			if err := tx.Put(objects, key, discoveredObject{Object: object, SeenAttempt: attempt.Attempt}); err != nil {
				return err
			}
		}
		if len(batch.Objects) > 0 {
			if current.LastBatch == math.MaxInt64 {
				return ErrInvalid
			}
			current.LastBatch++
			batch.Number = current.LastBatch
			if err := tx.Put(batches, batchKey(batch.ParentID, batch.Number), batch); err != nil {
				return err
			}
		}
		current.UpdatedAt = time.Now().UTC()
		return tx.Put(states, parentKey(current.Spec.ParentID), current)
	})
}

func (s *Scanner) scan(ctx context.Context, attempt State) {
	ctx = nodecontrol.WithRequestContext(ctx, nodecontrol.RequestContext{TaskID: attempt.Spec.ParentID, Generation: attempt.Generation})
	store, err := s.factory.OpenEndpoint(ctx, attempt.Spec.Source)
	if err != nil {
		s.finish(attempt, err)
		return
	}
	var manifest LegacyManifest
	if attempt.Spec.LegacyManifestID != "" {
		manifest, err = s.legacyHeader(attempt.Spec)
		if err != nil {
			_ = store.Close()
			s.finish(attempt, err)
			return
		}
	}
	missing := map[string]bool{}
	pending := make([]transferstore.Object, 0, s.options.MaxBatchObjects)
	var bytes int64
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.commitBatch(ctx, attempt, pending, missing); err != nil {
			return err
		}
		pending, bytes = pending[:0], 0
		clear(missing)
		return nil
	}
	emitAll := func(object transferstore.Object, retained, absent bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if object.Key == "" || object.Size < 0 {
			return ErrInvalid
		}
		// Local directory traversal need not be globally ordered by object key.
		// Skip out-of-range keys without terminating the source scan.
		if !retained && ((attempt.Spec.StartKey != "" && object.Key <= attempt.Spec.StartKey) ||
			(attempt.Spec.EndKey != "" && object.Key > attempt.Spec.EndKey)) {
			return nil
		}
		if attempt.Spec.LegacyManifestID != "" && !manifest.Exact {
			excluded, err := s.timeFiltered(attempt.Spec.ParentID, object.Key)
			if err != nil {
				return err
			}
			if excluded {
				return nil
			}
		}
		// V4 dateAfter is inclusive; dateBefore is exclusive. Retaining an old
		// discovered key does not exempt it from these scope filters. Missing
		// retained keys still need an explicit failure; their mtime is unknown.
		outside := (!attempt.Spec.ModifiedFrom.IsZero() && object.ModifiedAt.Before(attempt.Spec.ModifiedFrom)) ||
			(!attempt.Spec.ModifiedUntil.IsZero() && !object.ModifiedAt.Before(attempt.Spec.ModifiedUntil))
		if outside && !manifest.Exact && !absent {
			return s.store.View(func(tx *state.Tx) error {
				var prior discoveredObject
				found, err := tx.Get(objects, objectKey(attempt.Spec.ParentID, object.Key), &prior)
				if err != nil {
					return err
				}
				if found && !sameObject(prior.Object, object) {
					return changed(object.Key)
				}
				return nil
			})
		}
		// One oversized object is emitted alone. Object metadata is not split
		// into multipart pieces, and the scanner never reads the object body.
		if len(pending) > 0 && (len(pending) >= s.options.MaxBatchObjects || object.Size > s.options.MaxBatchBytes-bytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		missing[object.Key] = absent
		pending = append(pending, copyObject(object))
		bytes += object.Size
		if len(pending) >= s.options.MaxBatchObjects || bytes >= s.options.MaxBatchBytes {
			return flush()
		}
		return nil
	}
	emit := func(object transferstore.Object) error { return emitAll(object, false, false) }
	if attempt.Spec.InventoryID != "" {
		err = s.scanInventory(ctx, attempt, store, func(object transferstore.Object, absent bool) error { return emitAll(object, true, absent) })
	} else if manifest.Exact {
		// Only the original immutable inventory belongs to an inventory task.
	} else if attempt.Spec.SingleKey != "" {
		var object transferstore.Object
		object, err = store.Stat(ctx, attempt.Spec.SingleKey)
		if err == nil {
			err = emit(object)
		} else if errors.Is(err, transferstore.ErrNotFound) && attempt.Spec.LegacyManifestID != "" {
			// The retained inventory emits an explicit failed object below.
			err = nil
		}
	} else {
		err = store.Scan(ctx, transferstore.ListOptions{Prefix: attempt.Spec.Prefix, StartAfter: attempt.Spec.StartKey}, emit)
	}
	if err == nil {
		err = flush()
	}
	if err == nil && attempt.Spec.LegacyManifestID != "" {
		err = s.appendLegacy(ctx, attempt, store, func(object transferstore.Object, absent bool) error { return emitAll(object, true, absent) })
	}
	if err == nil {
		err = flush()
	}
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = ctx.Err()
	}
	s.finish(attempt, err)
}

func (s *Scanner) finish(attempt State, scanErr error) {
	err := s.store.Update(func(tx *state.Tx) error {
		current, err := get(tx, attempt.Spec.ParentID)
		if err != nil {
			return err
		}
		if current.Generation != attempt.Generation || current.Attempt != attempt.Attempt || current.Status != Scanning {
			return nil
		}
		current.UpdatedAt = time.Now().UTC()
		if scanErr != nil {
			current.Status = Failed
			if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
				current.Status = Recoverable
			}
			current.Error = scanErr.Error()
		} else {
			current.Status, current.ScanComplete = Completed, true
			current.MissingObjects = current.TotalObjects - current.SeenObjects
			if current.MissingObjects > 0 {
				current.Warning = fmt.Sprintf("%d previously discovered source objects were absent during rescan; retained batches are unchanged", current.MissingObjects)
			}
		}
		return tx.Put(states, parentKey(current.Spec.ParentID), current)
	})
	if err != nil {
		// Returning stale SCANNING or claiming completion after a failed commit
		// would conceal a broken durable checkpoint. Recovery reopens the database.
		s.faults.Store(attempt.Spec.ParentID, fmt.Errorf("persist discovery state: %w", err))
	}
}
