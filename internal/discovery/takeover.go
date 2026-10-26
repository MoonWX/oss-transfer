package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const takeoverBatches = "discovery-takeover-batches"
const MaxTakeoverBatchCount = 128
const MaxTakeoverPayloadBytes = 4 << 20

// TakeoverRequest replays only Master's durably assigned source manifests.
// It deliberately contains no source cursor: directory walks and resumed scan
// histories are not globally ordered by object key.
type TakeoverRequest struct {
	Spec         Spec    `json:"spec"`
	Epoch        uint64  `json:"epoch"`
	ThroughBatch int64   `json:"throughBatch"`
	Objects      int64   `json:"objects"`
	Bytes        int64   `json:"bytes"`
	Batches      []Batch `json:"batches,omitempty"`
}

type TakeoverState struct {
	ThroughBatch     int64 `json:"throughBatch"`
	Objects          int64 `json:"objects"`
	Bytes            int64 `json:"bytes"`
	SeededThrough    int64 `json:"seededThrough"`
	SeededObjects    int64 `json:"seededObjects"`
	SeededBytes      int64 `json:"seededBytes"`
	DuplicateObjects int64 `json:"duplicateObjects,omitempty"`
	Cleaning         bool  `json:"cleaning"`
	Sealed           bool  `json:"sealed"`
}

func requestedEpoch(values []uint64) uint64 {
	if len(values) > 0 {
		return values[0]
	}
	return 0
}

func (s *Scanner) parentContext(parentID int64, generation uint64) (context.Context, error) {
	if s.options.ParentContext == nil {
		return context.Background(), nil
	}
	ctx, err := s.options.ParentContext(parentID, generation)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrStopped
	}
	return ctx, ctx.Err()
}

func (s *Scanner) StatusEpoch(ctx context.Context, parentID int64, generation, epoch uint64) (State, error) {
	current, err := s.Status(ctx, parentID)
	if err == nil && current.Generation != generation {
		err = ErrGeneration
	}
	if err == nil && current.Epoch != epoch {
		err = ErrEpoch
	}
	return current, err
}

// QuiesceParent cancels I/O before waiting for persistence or releasing a lease.
// The caller must close admission first so another request cannot start a run.
func (s *Scanner) QuiesceParent(ctx context.Context, parentID int64) error {
	s.mu.Lock()
	run := s.active[parentID]
	if run != nil {
		run.cancel()
	}
	s.mu.Unlock()
	if run == nil {
		return ctx.Err()
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateTakeover(request TakeoverRequest) error {
	if !validSpec(request.Spec) || request.Spec.LegacyManifestID != "" || request.Epoch == 0 ||
		request.Spec.Epoch != 0 && request.Spec.Epoch != request.Epoch || request.ThroughBatch < 0 ||
		request.Objects < 0 || request.Bytes < 0 || request.ThroughBatch > request.Objects ||
		request.ThroughBatch == 0 && (request.Objects != 0 || request.Bytes != 0) || len(request.Batches) > MaxTakeoverBatchCount {
		return ErrInvalid
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(raw) > MaxTakeoverPayloadBytes {
		return fmt.Errorf("%w: takeover page exceeds 4 MiB", ErrCapacity)
	}
	for i, batch := range request.Batches {
		if batch.ParentID != request.Spec.ParentID || batch.Number < 1 || batch.Number > request.ThroughBatch ||
			len(batch.Objects) < 1 || len(batch.Objects) > 128 || len(batch.Legacy) != 0 ||
			i > 0 && batch.Number != request.Batches[i-1].Number+1 {
			return ErrInvalid
		}
		var bytes int64
		for _, object := range batch.Objects {
			if object.Key == "" || len(object.Key) > 1024 || !utf8.ValidString(object.Key) || strings.ContainsRune(object.Key, '\x00') ||
				object.Size < 0 || object.Size > math.MaxInt64-bytes {
				return ErrInvalid
			}
			bytes += object.Size
		}
		if bytes != batch.Bytes {
			return ErrInvalid
		}
	}
	return nil
}

func seedAttempt(epoch uint64) string { return fmt.Sprintf("takeover:%d", epoch) }

// SeedTakeover fences the previous local scan, clears its private scan-ahead in
// bounded transactions, and imports an idempotent page of assigned batches.
// No source is opened before FinalizeTakeover seals the complete replay.
func (s *Scanner) SeedTakeover(ctx context.Context, request TakeoverRequest) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := validateTakeover(request); err != nil {
		return State{}, err
	}
	request.Spec = normalized(request.Spec)
	request.Spec.Epoch = request.Epoch
	parent, err := s.parentContext(request.Spec.ParentID, request.Spec.Generation)
	if err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	current, err := s.Status(ctx, request.Spec.ParentID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return State{}, err
	}
	if err == nil {
		if current.Deleted {
			return State{}, ErrDeleted
		}
		if current.Generation > request.Spec.Generation || current.Epoch == request.Epoch && current.Generation != request.Spec.Generation {
			return State{}, ErrGeneration
		}
		if current.Epoch > request.Epoch {
			return State{}, ErrEpoch
		}
		if current.Epoch == request.Epoch {
			if current.Takeover == nil || !sameSpec(current.Spec, request.Spec) ||
				current.Takeover.ThroughBatch != request.ThroughBatch || current.Takeover.Objects != request.Objects || current.Takeover.Bytes != request.Bytes {
				return State{}, ErrConflict
			}
			if current.Status == Stopped {
				return State{}, ErrStopped
			}
		}
	}
	if current.Epoch != request.Epoch {
		if s.isActiveLocked(request.Spec.ParentID) {
			s.active[request.Spec.ParentID].cancel()
			return State{}, ErrRunning
		}
		current = State{Spec: request.Spec, Generation: request.Spec.Generation, Epoch: request.Epoch, Status: Seeding,
			Takeover: &TakeoverState{ThroughBatch: request.ThroughBatch, Objects: request.Objects, Bytes: request.Bytes, Cleaning: true}}
	}
	err = s.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := parent.Err(); err != nil {
			return err
		}
		progress := current.Takeover
		if progress.Cleaning {
			// One bounded page per transaction, including across process restarts.
			for _, bucket := range []string{objects, batches, takeoverBatches} {
				cleared, err := deleteCleanupPage(tx, bucket, parentKey(request.Spec.ParentID)+"/")
				if err != nil {
					return err
				}
				if !cleared {
					current.UpdatedAt = time.Now().UTC()
					return tx.Put(states, parentKey(request.Spec.ParentID), current)
				}
			}
			progress.Cleaning = false
		}
		for _, batch := range request.Batches {
			// Ignore old generation/epoch in the retained source manifest hash.
			batch.Generation, batch.Epoch = 0, 0
			raw, err := json.Marshal(batch)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			index := batchKey(request.Spec.ParentID, batch.Number)
			if batch.Number <= progress.SeededThrough {
				var prior string
				found, err := tx.Get(takeoverBatches, index, &prior)
				if err != nil {
					return err
				}
				if !found || prior != hash {
					return ErrConflict
				}
				continue
			}
			if progress.Sealed || batch.Number != progress.SeededThrough+1 {
				return ErrConflict
			}
			for _, object := range batch.Objects {
				prior, found, err := getDiscoveredObject(tx, request.Spec.ParentID, object.Key)
				if err != nil {
					return err
				}
				if found {
					if !sameObject(prior.Object, object) {
						return changed(object.Key)
					}
					progress.DuplicateObjects++
				} else if err := tx.Put(objects, objectKey(request.Spec.ParentID, object.Key), discoveredObject{Object: object, SeenAttempt: seedAttempt(request.Epoch)}); err != nil {
					return err
				}
			}
			if int64(len(batch.Objects)) > progress.Objects-progress.SeededObjects || batch.Bytes > progress.Bytes-progress.SeededBytes {
				return ErrConflict
			}
			progress.SeededObjects += int64(len(batch.Objects))
			progress.SeededBytes += batch.Bytes
			progress.SeededThrough = batch.Number
			if err := tx.Put(takeoverBatches, index, hash); err != nil {
				return err
			}
		}
		current.UpdatedAt = time.Now().UTC()
		return tx.Put(states, parentKey(request.Spec.ParentID), current)
	})
	return current, err
}

func (s *Scanner) FinalizeTakeover(ctx context.Context, parentID int64, generation, epoch uint64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if _, err := s.parentContext(parentID, generation); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	current, err := s.StatusEpoch(ctx, parentID, generation, epoch)
	if err != nil {
		return State{}, err
	}
	if current.Deleted {
		return State{}, ErrDeleted
	}
	if current.Status == Stopped {
		return State{}, ErrStopped
	}
	progress := current.Takeover
	if progress == nil || progress.Cleaning || progress.SeededThrough != progress.ThroughBatch ||
		progress.SeededObjects != progress.Objects || progress.SeededBytes != progress.Bytes {
		return State{}, ErrSeeding
	}
	if progress.Sealed && current.Status != Recoverable {
		return current, nil
	}
	if s.isActiveLocked(parentID) {
		return State{}, ErrRunning
	}
	if len(s.active) >= s.options.MaxConcurrentScans {
		return State{}, ErrCapacity
	}
	if current.Spec.InventoryID != "" {
		if err := inventory.New(s.store).Require(ctx, current.Spec.InventoryID, parentID); err != nil {
			return State{}, err
		}
	}
	if !progress.Sealed {
		progress.Sealed = true
		current.LastBatch, current.AckedThrough = progress.ThroughBatch, progress.ThroughBatch
		current.TotalObjects, current.TotalBytes = progress.Objects, progress.Bytes
	}
	return s.launchLocked(current)
}

func sameScanObject(attempt State, prior discoveredObject, object transferstore.Object) bool {
	if attempt.Takeover != nil && attempt.Spec.Source.Kind == "LOCAL" && prior.SeenAttempt == seedAttempt(attempt.Epoch) {
		// Master verified the shared volume before takeover. Device/inode based
		// ETags belong to the old mount; size, timestamp and metadata still fence
		// changes, matching shared-source execution on a different Agent.
		left, right := prior.Object, object
		left.ETag, right.ETag = "", ""
		return sameObject(left, right)
	}
	return sameObject(prior.Object, object)
}
