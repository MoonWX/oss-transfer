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
	keyOmitted  bool
}

type pendingBatch struct {
	objects    []transferstore.Object
	missing    map[string]bool
	checkpoint *Checkpoint
}

func scanIdentity(attempt State) string {
	if attempt.Checkpoint != nil {
		return attempt.Checkpoint.Pass
	}
	return attempt.Attempt
}

func objectKey(parentID int64, key string) string {
	return parentKey(parentID) + "/" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func sameObject(left, right transferstore.Object) bool {
	return left.Key == right.Key && left.Size == right.Size && transferstore.EqualETag(left.ETag, right.ETag) &&
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
	var absent map[string]bool
	if len(missing) > 0 {
		absent = missing[0]
	}
	return s.commitBatches(ctx, attempt, []pendingBatch{{objects: pending, missing: absent}})
}

func (s *Scanner) commitBatches(ctx context.Context, attempt State, pending []pendingBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	objectPrefix := parentKey(attempt.Spec.ParentID) + "/"
	return s.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := get(tx, attempt.Spec.ParentID)
		if err != nil {
			return err
		}
		if current.Generation != attempt.Generation || current.Epoch != attempt.Epoch || current.Attempt != attempt.Attempt {
			return ErrGeneration
		}
		if current.Status != Scanning {
			return ErrStopped
		}
		modified := false
		for _, part := range pending {
			batch := Batch{Checkpoint: current.TransferCheckpoint, ParentID: attempt.Spec.ParentID, Generation: current.Generation, Epoch: current.Epoch, Objects: make([]transferstore.Object, 0, len(part.objects))}
			for _, object := range part.objects {
				key := objectPrefix + base64.RawURLEncoding.EncodeToString([]byte(object.Key))
				prior, found, err := getDiscoveredObjectAtKey(tx, key, object.Key)
				if err != nil {
					return err
				}
				if found && !sameScanObject(attempt, prior, object) {
					if attempt.Spec.InventoryID == "" || !execution.SourceIdentityUnknown(prior.Object) {
						return changed(object.Key)
					}
					// Keep the original missing-key assignment on rescan. Execution
					// pins its first available identity before reading any data.
					object = prior.Object
				}
				if prior.SeenAttempt == scanIdentity(attempt) {
					continue
				}
				if !found {
					if current.TotalObjects == math.MaxInt64 || object.Size < 0 || object.Size > math.MaxInt64-current.TotalBytes {
						return ErrInvalid
					}
					candidate, err := s.legacyCandidate(tx, attempt.Spec, object, part.missing[object.Key])
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
				seededResume := attempt.Takeover != nil && attempt.Takeover.Checkpoint != nil && prior.SeenAttempt == seedAttempt(attempt.Epoch)
				if !seededResume {
					current.SeenObjects++
				}
				modified = true
				if err := tx.Put(objects, key, discoveredObject{Object: object, SeenAttempt: scanIdentity(attempt)}); err != nil {
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
			if part.checkpoint != nil {
				if part.checkpoint.Version != 1 || part.checkpoint.Pass != scanIdentity(attempt) {
					return ErrCorrupt
				}
				checkpoint := *part.checkpoint
				current.Checkpoint = &checkpoint
				if attempt.Spec.LegacyManifestID == "" {
					sequence := uint64(1)
					if current.TransferCheckpoint != nil {
						sequence = current.TransferCheckpoint.Sequence + 1
					}
					if sequence == 0 {
						return ErrCorrupt
					}
					recovery := &RecoveryCheckpoint{Version: 1, Epoch: current.Epoch, Sequence: sequence, ThroughBatch: current.LastBatch,
						Objects: current.TotalObjects, Bytes: current.TotalBytes, Cursor: checkpoint.Cursor,
						SourceDone: checkpoint.SourceDone, SourceRoot: attempt.Spec.Source.Root}
					current.TransferCheckpoint = recovery
					// Attach the boundary to the last output batch atomically with the
					// source page. A concurrent reader cannot observe it ahead of data.
					// Later batches also carry this boundary in case this batch was
					// assigned before the checkpoint-only commit reached the store.
					if current.LastBatch > current.ReleasedThrough {
						var tail Batch
						found, err := tx.Get(batches, batchKey(attempt.Spec.ParentID, current.LastBatch), &tail)
						if err != nil {
							return err
						}
						if found {
							tail.Checkpoint = recovery
							if err := tx.Put(batches, batchKey(tail.ParentID, tail.Number), tail); err != nil {
								return err
							}
						}
					}
				}
				modified = true
			}
		}
		if !modified {
			return nil
		}
		current.UpdatedAt = time.Now().UTC()
		return tx.Put(states, parentKey(current.Spec.ParentID), current)
	})
}

func (s *Scanner) scan(ctx context.Context, attempt State) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = nodecontrol.WithRequestContext(ctx, nodecontrol.RequestContext{TaskID: attempt.Spec.ParentID, Generation: attempt.Generation})
	if attempt.Checkpoint != nil && attempt.Checkpoint.SourceDone &&
		(attempt.Spec.LegacyManifestID == "" || attempt.Checkpoint.LegacyDone) {
		s.finish(attempt, ctx.Err())
		return
	}
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
	pageSource, resumable := store.(transferstore.CheckpointScanner)
	if resumable || attempt.Spec.InventoryID != "" || manifest.Exact || attempt.Spec.SingleKey != "" {
		if err = s.beginCheckpoint(ctx, &attempt); err != nil {
			_ = store.Close()
			s.finish(attempt, err)
			return
		}
	} else if attempt.Checkpoint != nil {
		_ = store.Close()
		s.finish(attempt, fmt.Errorf("%w: source cannot resume its saved scan checkpoint", transferstore.ErrUnsupported))
		return
	}
	missing := map[string]bool{}
	pending := make([]transferstore.Object, 0, s.options.MaxBatchObjects)
	var bytes int64
	committer := newScanCommitter(ctx, cancel, func(batches []pendingBatch) error {
		return s.commitBatches(ctx, attempt, batches)
	})
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := committer.add(pendingBatch{objects: pending, missing: missing}); err != nil {
			return err
		}
		pending, bytes = make([]transferstore.Object, 0, s.options.MaxBatchObjects), 0
		missing = map[string]bool{}
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
				prior, found, err := getDiscoveredObject(tx, attempt.Spec.ParentID, object.Key)
				if err != nil {
					return err
				}
				if found && !sameScanObject(attempt, prior, object) {
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
	checkpoint := func(cursor string, done bool) error {
		if err := flush(); err != nil {
			return err
		}
		progress := *attempt.Checkpoint
		progress.Cursor, progress.SourceDone = cursor, done
		return committer.add(pendingBatch{checkpoint: &progress})
	}
	if attempt.Checkpoint != nil && attempt.Checkpoint.SourceDone {
		// A crash after the last page does not reopen the source enumeration.
	} else if attempt.Spec.InventoryID != "" {
		err = s.scanInventory(ctx, attempt, store, func(object transferstore.Object, absent bool) error { return emitAll(object, true, absent) }, checkpoint)
	} else if manifest.Exact {
		// Only the original immutable inventory belongs to an inventory task.
		err = checkpoint("", true)
	} else if attempt.Spec.SingleKey != "" {
		var object transferstore.Object
		object, err = store.Stat(ctx, attempt.Spec.SingleKey)
		if err == nil {
			err = emit(object)
		} else if errors.Is(err, transferstore.ErrNotFound) && attempt.Spec.LegacyManifestID != "" {
			// The retained inventory emits an explicit failed object below.
			err = nil
		}
		if err == nil {
			err = checkpoint("", true)
		}
	} else if resumable {
		err = pageSource.ScanPages(ctx, transferstore.ListOptions{Prefix: attempt.Spec.Prefix, StartAfter: attempt.Spec.StartKey, Cursor: attempt.Checkpoint.Cursor}, func(page transferstore.Page) error {
			for _, object := range page.Objects {
				if err := emit(object); err != nil {
					return err
				}
			}
			return checkpoint(page.NextCursor, page.NextCursor == "")
		})
	} else {
		err = store.Scan(ctx, transferstore.ListOptions{Prefix: attempt.Spec.Prefix, StartAfter: attempt.Spec.StartKey}, emit)
	}
	if err == nil {
		err = flush()
	}
	if err == nil && attempt.Spec.LegacyManifestID != "" {
		// Legacy reconciliation reads the durable SeenAttempt index to avoid
		// re-HEADing objects already emitted by LIST. Publish that phase first.
		err = committer.close()
		if err == nil {
			committer = newScanCommitter(ctx, cancel, func(batches []pendingBatch) error {
				return s.commitBatches(ctx, attempt, batches)
			})
			err = s.appendLegacy(ctx, attempt, store, func(object transferstore.Object, absent bool) error { return emitAll(object, true, absent) }, func(after string, done bool) error {
				if attempt.Checkpoint == nil {
					return nil
				}
				if err := flush(); err != nil {
					return err
				}
				progress := *attempt.Checkpoint
				progress.Cursor, progress.SourceDone = "", true
				progress.LegacyAfter, progress.LegacyDone = after, done
				return committer.add(pendingBatch{checkpoint: &progress})
			})
		}
	}
	if err == nil {
		err = flush()
	}
	if commitErr := committer.close(); commitErr != nil {
		err = commitErr
	}
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = ctx.Err()
	}
	s.finish(attempt, err)
}

func (s *Scanner) beginCheckpoint(ctx context.Context, attempt *State) error {
	if attempt.Checkpoint != nil {
		return nil
	}
	checkpoint := &Checkpoint{Version: 1, Pass: attempt.Attempt}
	err := s.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := get(tx, attempt.Spec.ParentID)
		if err != nil {
			return err
		}
		if current.Generation != attempt.Generation || current.Epoch != attempt.Epoch || current.Attempt != attempt.Attempt {
			return ErrGeneration
		}
		if current.Status != Scanning {
			return ErrStopped
		}
		current.Checkpoint = checkpoint
		return tx.Put(states, parentKey(attempt.Spec.ParentID), current)
	})
	if err == nil {
		attempt.Checkpoint = checkpoint
	}
	return err
}

func (s *Scanner) finish(attempt State, scanErr error) {
	err := s.store.Update(func(tx *state.Tx) error {
		current, err := get(tx, attempt.Spec.ParentID)
		if err != nil {
			return err
		}
		if current.Generation != attempt.Generation || current.Epoch != attempt.Epoch || current.Attempt != attempt.Attempt || current.Status != Scanning {
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
		if err := tx.Put(states, parentKey(current.Spec.ParentID), current); err != nil {
			return err
		}
		if cleanupNeeded(current) {
			return tx.Put(cleanupBucket, parentKey(current.Spec.ParentID), current.Spec.ParentID)
		}
		return nil
	})
	if err != nil {
		// Returning stale SCANNING or claiming completion after a failed commit
		// would conceal a broken durable checkpoint. Recovery reopens the database.
		s.faults.Store(attempt.Spec.ParentID, fmt.Errorf("persist discovery state: %w", err))
	} else {
		s.signalCleanup()
	}
}
