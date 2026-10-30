package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const states = "discovery-states"
const batches = "discovery-batches"
const objects = "discovery-objects"

type running struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Scanner struct {
	store         *state.Store
	factory       EndpointFactory
	options       Options
	mu            sync.Mutex
	active        map[int64]*running
	faults        sync.Map // parent ID -> terminal-state persistence error; requires recovery.
	closed        bool
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
	cleanupWake   chan struct{}
}

func (s *Scanner) Concurrency() (active, capacity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active), s.options.MaxConcurrentScans
}

func New(store *state.Store, factory EndpointFactory, options Options) (*Scanner, error) {
	if store == nil || factory == nil || options.MaxConcurrentScans < 0 || options.MaxBatchObjects < 0 || options.MaxBatchObjects > 128 || options.MaxBatchBytes < 0 || options.MaxBatchBytes > 256<<20 {
		return nil, ErrInvalid
	}
	if options.MaxConcurrentScans == 0 {
		options.MaxConcurrentScans = 2
	}
	if options.MaxBatchObjects == 0 {
		options.MaxBatchObjects = 128
	}
	if options.MaxBatchBytes == 0 {
		options.MaxBatchBytes = 256 << 20
	}
	s := &Scanner{store: store, factory: factory, options: options, active: make(map[int64]*running)}
	// A restarted process has no live scan. Preserve committed batches and make
	// recovery explicit; an empty in-memory worker map never means completion.
	after := ""
	for {
		count := 0
		err := store.Update(func(tx *state.Tx) error {
			return tx.Scan(states, "", after, 128, func(key string, raw json.RawMessage) error {
				count++
				after = key
				var current State
				if err := json.Unmarshal(raw, &current); err != nil {
					return err
				}
				if current.Status == Scanning {
					current.Status = Recoverable
					current.Error = "Discovery process stopped before the scan completed"
					current.UpdatedAt = time.Now().UTC()
					if err := tx.Put(states, key, current); err != nil {
						return err
					}
				}
				if cleanupNeeded(current) {
					return tx.Put(cleanupBucket, key, current.Spec.ParentID)
				}
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		if count < 128 {
			s.startCleanup()
			return s, nil
		}
	}
}

func parentKey(parentID int64) string { return fmt.Sprintf("%020d", parentID) }
func batchKey(parentID, number int64) string {
	return fmt.Sprintf("%020d/%020d", parentID, number)
}

func validSpec(spec Spec) bool {
	if spec.InventoryID != "" && (!inventory.ValidID(spec.InventoryID) || spec.LegacyManifestID != "" || spec.Prefix != "" || spec.SingleKey != "" || spec.StartKey != "" || spec.EndKey != "" || !spec.ModifiedFrom.IsZero() || !spec.ModifiedUntil.IsZero()) {
		return false
	}
	if spec.ParentID < 1 || spec.Generation < 1 || (spec.SingleKey != "" && (spec.Prefix != "" || spec.StartKey != "" || spec.EndKey != "")) ||
		(spec.StartKey != "" && spec.EndKey != "" && spec.EndKey < spec.StartKey) ||
		(!spec.ModifiedFrom.IsZero() && !spec.ModifiedUntil.IsZero() && spec.ModifiedUntil.Before(spec.ModifiedFrom)) {
		return false
	}
	for _, key := range []string{spec.Prefix, spec.SingleKey, spec.StartKey, spec.EndKey} {
		if len(key) > 1024 || !utf8.ValidString(key) || strings.ContainsRune(key, '\x00') {
			return false
		}
	}
	switch spec.Source.Kind {
	case "S3":
		return spec.Source.Ref != "" && spec.Source.Bucket != "" && spec.Source.Root == ""
	case "LOCAL":
		return filepath.IsAbs(spec.Source.Root) && !strings.ContainsRune(spec.Source.Root, '\x00') && spec.Source.Ref == "" && spec.Source.Bucket == ""
	default:
		return false
	}
}

func normalized(spec Spec) Spec {
	spec.ModifiedFrom = spec.ModifiedFrom.UTC()
	spec.ModifiedUntil = spec.ModifiedUntil.UTC()
	return spec
}

func sameSpec(left, right Spec) bool {
	left, right = normalized(left), normalized(right)
	left.Generation, right.Generation = 0, 0
	return reflect.DeepEqual(left, right)
}

func get(tx *state.Tx, parentID int64) (State, error) {
	var current State
	found, err := tx.Get(states, parentKey(parentID), &current)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{}, ErrNotFound
	}
	return current, nil
}

func (s *Scanner) Status(ctx context.Context, parentID int64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	var current State
	err := s.store.View(func(tx *state.Tx) error {
		var err error
		current, err = get(tx, parentID)
		return err
	})
	if fault, found := s.faults.Load(parentID); err == nil && found {
		err = fault.(error)
	}
	return current, err
}

func (s *Scanner) Start(ctx context.Context, spec Spec) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if !validSpec(spec) {
		return State{}, ErrInvalid
	}
	if _, err := s.parentContext(spec.ParentID, spec.Generation, spec); err != nil {
		return State{}, err
	}
	if spec.InventoryID != "" {
		if err := inventory.New(s.store).Require(ctx, spec.InventoryID, spec.ParentID); err != nil {
			return State{}, err
		}
	}
	spec = normalized(spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	current, err := s.Status(ctx, spec.ParentID)
	if err == nil {
		if current.Deleted {
			return State{}, ErrDeleted
		}
		if current.Generation != spec.Generation {
			return State{}, ErrGeneration
		}
		if current.Epoch != spec.Epoch {
			return State{}, ErrEpoch
		}
		if current.Status == Stopped {
			return State{}, ErrStopped
		}
		if emptyFence(current) && current.Status == Recoverable {
			current.Spec = spec
		} else if !sameSpec(current.Spec, spec) {
			return State{}, ErrConflict
		}
		if current.Status == Recoverable && !s.isActiveLocked(spec.ParentID) {
			if len(s.active) >= s.options.MaxConcurrentScans {
				return State{}, ErrCapacity
			}
			return s.launchLocked(current)
		}
		// Lost responses and repeated calls must not open a second source scan.
		return current, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return State{}, err
	}
	if len(s.active) >= s.options.MaxConcurrentScans {
		return State{}, ErrCapacity
	}
	return s.launchLocked(State{Spec: spec, Generation: spec.Generation, Epoch: spec.Epoch})
}

func emptyFence(current State) bool {
	return current.Spec.Source.Kind == "" && current.TotalObjects == 0 && current.LastBatch == 0
}

func (s *Scanner) isActiveLocked(parentID int64) bool {
	run := s.active[parentID]
	if run == nil {
		return false
	}
	select {
	case <-run.done:
		delete(s.active, parentID)
		return false
	default:
		return true
	}
}

// launchLocked publishes the attempt before spawning it. Its context belongs to
// Scanner, so the short Start/Resume HTTP request can finish independently.
func (s *Scanner) launchLocked(current State) (State, error) {
	parent, err := s.parentContext(current.Spec.ParentID, current.Generation, current.Spec)
	if err != nil {
		return State{}, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return State{}, err
	}
	current.Attempt = hex.EncodeToString(token[:])
	current.Status, current.ScanComplete = Scanning, false
	if current.Checkpoint != nil {
		if current.Checkpoint.Version != 1 || current.Checkpoint.Pass == "" {
			return State{}, ErrCorrupt
		}
	} else {
		current.SeenObjects = 0
	}
	if current.Checkpoint == nil && current.Takeover != nil {
		// Identical historical duplicate assignments remain in Master counters,
		// while the recovery index stores each source key only once.
		current.SeenObjects = current.Takeover.DuplicateObjects
	}
	current.Error, current.Warning, current.MissingObjects = "", "", 0
	current.UpdatedAt = time.Now().UTC()
	if err := s.store.Update(func(tx *state.Tx) error { return tx.Put(states, parentKey(current.Spec.ParentID), current) }); err != nil {
		return State{}, err
	}
	ctx, cancel := context.WithCancel(parent)
	run := &running{cancel: cancel, done: make(chan struct{})}
	s.active[current.Spec.ParentID] = run
	go func() {
		defer func() {
			cancel()
			close(run.done)
			s.mu.Lock()
			if s.active[current.Spec.ParentID] == run {
				delete(s.active, current.Spec.ParentID)
			}
			s.mu.Unlock()
		}()
		s.scan(ctx, current)
	}()
	return current, nil
}

func (s *Scanner) Stop(ctx context.Context, parentID int64, generation uint64, epochs ...uint64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if parentID < 1 || generation < 1 {
		return State{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	var current State
	err := s.store.Update(func(tx *state.Tx) error {
		var err error
		current, err = get(tx, parentID)
		if errors.Is(err, ErrNotFound) {
			current = State{Spec: Spec{ParentID: parentID, Generation: generation, Epoch: requestedEpoch(epochs)}, Generation: generation, Epoch: requestedEpoch(epochs)}
		} else if err != nil {
			return err
		}
		if current.Deleted {
			return ErrDeleted
		}
		if current.Epoch != requestedEpoch(epochs) {
			return ErrEpoch
		}
		// A cluster Resume can reach only some Agents before a new Stop arrives.
		// A newer stop closes that generation without ever starting discovery.
		if generation < current.Generation {
			return ErrGeneration
		}
		current.Generation, current.Spec.Generation = generation, generation
		if current.Status != Completed {
			current.Status = Stopped
		}
		current.UpdatedAt = time.Now().UTC()
		return tx.Put(states, parentKey(parentID), current)
	})
	if err != nil {
		return State{}, err
	}
	if run := s.active[parentID]; run != nil {
		run.cancel()
	}
	return current, nil
}

func (s *Scanner) Resume(ctx context.Context, parentID int64, generation uint64, epochs ...uint64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if _, err := s.parentContext(parentID, generation); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return State{}, ErrClosed
	}
	current, err := s.Status(ctx, parentID)
	if err != nil {
		s.mu.Unlock()
		return State{}, err
	}
	if current.Deleted {
		s.mu.Unlock()
		return State{}, ErrDeleted
	}
	if current.Epoch != requestedEpoch(epochs) {
		s.mu.Unlock()
		return State{}, ErrEpoch
	}
	if generation == current.Generation && (current.Status == Scanning || current.Status == Seeding || current.Status == Completed || (emptyFence(current) && current.Status == Recoverable)) {
		s.mu.Unlock()
		return current, nil
	}
	if generation <= current.Generation {
		s.mu.Unlock()
		return State{}, ErrGeneration
	}
	if current.Status == Scanning {
		s.mu.Unlock()
		return State{}, ErrRunning
	}
	if !validSpec(current.Spec) && !emptyFence(current) {
		s.mu.Unlock()
		return State{}, ErrInvalid
	}
	run := s.active[parentID]
	if run != nil {
		run.cancel()
	}
	s.mu.Unlock()
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			return State{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	latest, err := s.Status(ctx, parentID)
	if err != nil {
		return State{}, err
	}
	if latest.Deleted {
		return State{}, ErrDeleted
	}
	if latest.Epoch != requestedEpoch(epochs) {
		return State{}, ErrEpoch
	}
	if generation == latest.Generation && (latest.Status == Scanning || latest.Status == Completed || (emptyFence(latest) && latest.Status == Recoverable)) {
		return latest, nil
	}
	if generation <= latest.Generation || latest.Generation != current.Generation {
		return State{}, ErrGeneration
	}
	latest.Spec.Generation, latest.Generation = generation, generation
	if emptyFence(latest) || latest.ScanComplete || latest.Takeover != nil && !latest.Takeover.Sealed {
		if latest.Takeover != nil && !latest.Takeover.Sealed {
			latest.Status = Seeding
		}
		if emptyFence(latest) {
			latest.Status = Recoverable
		}
		latest.UpdatedAt = time.Now().UTC()
		if err := s.store.Update(func(tx *state.Tx) error { return tx.Put(states, parentKey(parentID), latest) }); err != nil {
			return State{}, err
		}
		return latest, nil
	}
	s.isActiveLocked(parentID)
	if len(s.active) >= s.options.MaxConcurrentScans && s.active[parentID] == nil {
		return State{}, ErrCapacity
	}
	return s.launchLocked(latest)
}

func (s *Scanner) Batch(ctx context.Context, parentID int64, generation uint64, afterNumber int64, epochs ...uint64) (Batch, bool, error) {
	if err := ctx.Err(); err != nil {
		return Batch{}, false, err
	}
	if afterNumber < 0 {
		return Batch{}, false, ErrInvalid
	}
	var result Batch
	found := false
	err := s.store.View(func(tx *state.Tx) error {
		current, err := get(tx, parentID)
		if err != nil {
			return err
		}
		if current.Deleted {
			return ErrDeleted
		}
		if generation != current.Generation {
			return ErrGeneration
		}
		if current.Epoch != requestedEpoch(epochs) {
			return ErrEpoch
		}
		if current.Takeover != nil && (!current.Takeover.Sealed || afterNumber < current.Takeover.ThroughBatch) {
			return ErrSeeding
		}
		if afterNumber < current.ReleasedThrough {
			return ErrBatchReleased
		}
		if afterNumber >= current.LastBatch {
			return nil
		}
		found, err = tx.Get(batches, batchKey(parentID, afterNumber+1), &result)
		if err != nil {
			return err
		}
		if !found {
			return ErrCorrupt
		}
		result.Generation = current.Generation
		result.Epoch = current.Epoch
		return nil
	})
	return result, found, err
}

// Ack confirms delivery to Master without authorizing removal. A Master may
// still roll back its database until it has durably fenced that possibility.
func (s *Scanner) Ack(ctx context.Context, parentID int64, generation uint64, throughNumber int64, epochs ...uint64) (State, error) {
	return s.advanceBatchWatermark(ctx, parentID, generation, throughNumber, false, epochs...)
}

// Release authorizes cleanup of an already acknowledged prefix. Callers must
// first make that prefix survive Master rollback; old Masters never send this.
func (s *Scanner) Release(ctx context.Context, parentID int64, generation uint64, throughNumber int64, epochs ...uint64) (State, error) {
	return s.advanceBatchWatermark(ctx, parentID, generation, throughNumber, true, epochs...)
}

func (s *Scanner) advanceBatchWatermark(ctx context.Context, parentID int64, generation uint64, throughNumber int64, release bool, epochs ...uint64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if throughNumber < 0 {
		return State{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, ErrClosed
	}
	var current State
	err := s.store.Update(func(tx *state.Tx) error {
		var err error
		current, err = get(tx, parentID)
		if err != nil {
			return err
		}
		if current.Deleted {
			return ErrDeleted
		}
		if generation != current.Generation {
			return ErrGeneration
		}
		if current.Epoch != requestedEpoch(epochs) {
			return ErrEpoch
		}
		watermark, limit := &current.AckedThrough, current.LastBatch
		if release {
			watermark, limit = &current.ReleasableThrough, current.AckedThrough
		}
		if throughNumber > limit {
			return ErrInvalid
		}
		if throughNumber <= *watermark {
			return nil
		}
		*watermark, current.UpdatedAt = throughNumber, time.Now().UTC()
		if err := tx.Put(states, parentKey(parentID), current); err != nil {
			return err
		}
		if release {
			return tx.Put(cleanupBucket, parentKey(parentID), parentID)
		}
		return nil
	})
	if err == nil && release {
		s.signalCleanup()
	}
	return current, err
}

func (s *Scanner) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.cleanupCancel != nil {
		s.cleanupCancel()
	}
	active := make([]*running, 0, len(s.active))
	for _, run := range s.active {
		run.cancel()
		active = append(active, run)
	}
	s.mu.Unlock()
	for _, run := range active {
		<-run.done
	}
	if s.cleanupDone != nil {
		<-s.cleanupDone
	}
	var failures []error
	s.faults.Range(func(_, value any) bool {
		failures = append(failures, value.(error))
		return true
	})
	return errors.Join(failures...)
}
