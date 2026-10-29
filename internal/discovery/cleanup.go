package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const cleanupBucket = "discovery-cleanup"
const cleanupPageKeys = 2048
const cleanupPageBytes = 8 << 20
const cleanupInterval = 50 * time.Millisecond

func cleanupNeeded(current State) bool {
	return current.Deleted && !current.Purged || current.ScanComplete && !current.ObjectsReleased || current.ReleasedThrough < current.ReleasableThrough
}

func (s *Scanner) startCleanup() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cleanupCancel = cancel
	s.cleanupDone = make(chan struct{})
	s.cleanupWake = make(chan struct{}, 1)
	go s.cleanupLoop(ctx)
}

func (s *Scanner) signalCleanup() {
	select {
	case s.cleanupWake <- struct{}{}:
	default:
	}
}

// Sweep one bounded transaction at a time. Round-robin selection prevents a
// large old manifest from blocking cleanup of newer tasks. A persisted queue
// entry is removed only with the final cleanup commit.
func (s *Scanner) cleanupLoop(ctx context.Context) {
	defer close(s.cleanupDone)
	var after string
	var lastError time.Time
	for ctx.Err() == nil {
		var parentID int64
		var found bool
		err := s.store.View(func(tx *state.Tx) error {
			return tx.Scan(cleanupBucket, "", after, 1, func(key string, raw json.RawMessage) error {
				after, found = key, true
				return json.Unmarshal(raw, &parentID)
			})
		})
		if err == nil && !found && after != "" {
			after = ""
			continue
		}
		if err == nil && found {
			_, err = s.cleanupPage(ctx, parentID)
			if err != nil && !errors.Is(err, context.Canceled) {
				s.cleanupProblem(parentID, err)
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) && time.Since(lastError) >= time.Minute {
			log.Printf("discovery metadata cleanup will retry: %v", err)
			lastError = time.Now()
		}
		delay := cleanupInterval
		if err != nil || !found {
			delay = time.Second
		}
		// Coalesce ACKs arriving during an active sweep. Only an idle worker
		// may skip its delay; otherwise every ACK could add a durable commit.
		var wake <-chan struct{}
		if !found {
			wake = s.cleanupWake
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// RequestDeleteParent persists a fence before cancelling a live scan. Its I/O
// and metadata reclamation are drained by the scanner-owned background worker.
func (s *Scanner) RequestDeleteParent(ctx context.Context, parentID int64, generation uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parentID < 1 || generation < 1 {
		return ErrInvalid
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	err := s.store.Update(func(tx *state.Tx) error {
		current, err := get(tx, parentID)
		if errors.Is(err, ErrNotFound) {
			current = State{Spec: Spec{ParentID: parentID, Generation: generation}, Generation: generation}
		} else if err != nil {
			return err
		}
		if generation < current.Generation {
			return ErrGeneration
		}
		if current.Deleted && current.Generation == generation {
			return nil
		}
		current.Generation, current.Spec.Generation = generation, generation
		current.Deleted, current.Status = true, Stopped
		current.UpdatedAt = time.Now().UTC()
		if err := tx.Put(states, parentKey(parentID), current); err != nil {
			return err
		}
		if current.Purged {
			return nil
		}
		return tx.Put(cleanupBucket, parentKey(parentID), parentID)
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.faults.Delete(parentID)
	if run := s.active[parentID]; run != nil {
		run.cancel()
	}
	s.mu.Unlock()
	s.signalCleanup()
	return nil
}

// DeleteParent is the synchronous compatibility path. A cancelled waiter does
// not cancel durable cleanup; RequestDeleteParent is used by new Masters.
func (s *Scanner) DeleteParent(ctx context.Context, parentID int64, generation uint64) error {
	if err := s.RequestDeleteParent(ctx, parentID, generation); err != nil {
		return err
	}
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		current, err := s.Status(ctx, parentID)
		if err != nil || current.Purged {
			return err
		}
		if current.CleanupError != "" {
			return errors.New(current.CleanupError)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.cleanupDone:
			return ErrClosed
		case <-timer.C:
		}
	}
}

func (s *Scanner) cleanupProblem(parentID int64, problem error) {
	_ = s.store.Update(func(tx *state.Tx) error {
		current, err := get(tx, parentID)
		if err != nil || !current.Deleted || current.Purged {
			return err
		}
		current.CleanupError = problem.Error()
		if len(current.CleanupError) > 4096 {
			current.CleanupError = current.CleanupError[:4096]
		}
		return tx.Put(states, parentKey(parentID), current)
	})
}

func (s *Scanner) cleanupPage(ctx context.Context, parentID int64) (done bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.isActiveLocked(parentID)
	// Lifecycle methods snapshot State under this mutex before publishing a
	// new attempt. Keep their read/write window separate from batch deletion.
	err = s.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := get(tx, parentID)
		if err != nil {
			return err
		}
		if !cleanupNeeded(current) {
			done = true
			return tx.Delete(cleanupBucket, parentKey(parentID))
		}
		// Master explicitly permits this acknowledged prefix to be released.
		// Discovery may still be using its source recovery index.
		if !current.Deleted && current.ReleasedThrough < current.ReleasableThrough {
			if err := releaseAckedBatchesPage(tx, &current); err != nil {
				return err
			}
			if err := tx.Put(states, parentKey(parentID), current); err != nil {
				return err
			}
			if cleanupNeeded(current) {
				return nil // Keep each write transaction bounded to one batch page.
			}
			done = true
			return tx.Delete(cleanupBucket, parentKey(parentID))
		}
		if active {
			return nil
		}
		current.CleanupError = ""
		if current.Deleted {
			buckets := []string{objects, batches, legacyKeys, takeoverBatches}
			if current.ObjectsReleased && current.CleanupPhase == 0 {
				current.CleanupPhase = 1
				current.CleanupAfter = ""
			}
			if current.CleanupPhase < len(buckets) {
				phase := current.CleanupPhase
				page, err := tx.DeletePrefixPage(ctx, buckets[phase], parentKey(parentID)+"/", current.CleanupAfter, state.DefaultCleanupBudget())
				if err != nil {
					return err
				}
				current.CleanupDeleted += page.Deleted
				current.CleanupAfter = page.After
				if page.Done {
					current.CleanupPhase++
					current.CleanupAfter = ""
					if phase == 0 {
						current.ObjectsReleased = true
					}
				}
				return tx.Put(states, parentKey(parentID), current)
			}
			if err := tx.Delete(legacyManifest, parentKey(parentID)); err != nil {
				return err
			}
			current = State{Spec: Spec{ParentID: parentID, Generation: current.Generation, Epoch: current.Epoch}, Generation: current.Generation, Epoch: current.Epoch,
				Status: Stopped, Deleted: true, Purged: true, ObjectsReleased: true, UpdatedAt: time.Now().UTC(), CleanupDeleted: current.CleanupDeleted}
		} else if !current.ObjectsReleased && current.ScanComplete {
			page, err := tx.DeletePrefixPage(ctx, objects, parentKey(parentID)+"/", current.CleanupAfter, state.DefaultCleanupBudget())
			if err != nil {
				return err
			}
			current.CleanupAfter = page.After
			if page.Done {
				current.ObjectsReleased = true
				current.CleanupAfter = ""
			}
			if err := tx.Put(states, parentKey(parentID), current); err != nil {
				return err
			}
			if !page.Done {
				return nil
			}
		}
		if err := tx.Put(states, parentKey(parentID), current); err != nil {
			return err
		}
		done = true
		return tx.Delete(cleanupBucket, parentKey(parentID))
	})
	return
}

var errCleanupPageFull = errors.New("cleanup page full")

// Compatibility for takeover cleanup, which already commits one page per call.
func deleteCleanupPage(tx *state.Tx, bucket, prefix string) (bool, error) {
	page, err := tx.DeletePrefixPage(context.Background(), bucket, prefix, "", state.DefaultCleanupBudget())
	return page.Done, err
}

// releaseAckedBatchesPage only touches the prefix explicitly released by
// Master. The watermark and deletions commit together, including the
// minimum reader version: an interrupted transaction cannot lose its cursor.
func releaseAckedBatchesPage(tx *state.Tx, current *State) error {
	through := current.ReleasedThrough
	if current.Takeover != nil && current.Takeover.Sealed {
		// Seeded assignments live on Master, not in this scanner's batch bucket.
		through = max(through, min(current.Takeover.ThroughBatch, current.ReleasableThrough))
	}
	if through > current.ReleasableThrough || current.ReleasableThrough > current.AckedThrough || current.AckedThrough > current.LastBatch {
		return ErrCorrupt
	}
	keys := make([]string, 0, cleanupPageKeys)
	var bytes int64
	started := time.Now()
	err := tx.ScanKeys(batches, parentKey(current.Spec.ParentID)+"/", batchKey(current.Spec.ParentID, through), cleanupPageKeys+1, func(key string, storedBytes int64) error {
		if through == current.ReleasableThrough {
			return errCleanupPageFull
		}
		if key != batchKey(current.Spec.ParentID, through+1) {
			return ErrCorrupt
		}
		remaining := cleanupPageBytes - bytes
		fits := storedBytes <= remaining
		if len(keys) == cleanupPageKeys || len(keys) > 0 && (!fits || time.Since(started) >= state.DefaultCleanupBudget().MaxDuration) {
			return errCleanupPageFull
		}
		keys = append(keys, key)
		through++
		if fits {
			bytes += storedBytes
		} else {
			bytes = cleanupPageBytes
		}
		return nil
	})
	full := errors.Is(err, errCleanupPageFull)
	if err != nil && !full {
		return err
	}
	if !full && through < current.ReleasableThrough {
		return ErrCorrupt
	}
	if len(keys) > 0 {
		if err := state.RequireSchema(tx, "agent", state.SchemaReleasedScanBatches); err != nil {
			return err
		}
		for _, key := range keys {
			if err := tx.Delete(batches, key); err != nil {
				return err
			}
		}
	}
	current.ReleasedThrough = through
	return nil
}
