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
const cleanupPageKeys = 1024
const cleanupPageBytes = 1 << 20
const cleanupInterval = 50 * time.Millisecond

func cleanupNeeded(current State) bool {
	return current.Deleted && !current.Purged || current.ScanComplete && !current.ObjectsReleased
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
		}
		if err != nil && !errors.Is(err, context.Canceled) && time.Since(lastError) >= time.Minute {
			log.Printf("discovery metadata cleanup will retry: %v", err)
			lastError = time.Now()
		}
		delay := cleanupInterval
		if err != nil || !found {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-s.cleanupWake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// DeleteParent permanently fences discovery before waiting for its source
// handle to close. Retrying after cancellation or a restart finishes cleanup;
// ordinary Stop/Resume never removes the manifest or its recovery index.
func (s *Scanner) DeleteParent(ctx context.Context, parentID int64, generation uint64) error {
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
		if current.Deleted && current.Purged && current.Generation == generation {
			return nil
		}
		current.Generation, current.Spec.Generation = generation, generation
		current.Deleted, current.Status = true, Stopped
		current.UpdatedAt = time.Now().UTC()
		if err := tx.Put(states, parentKey(parentID), current); err != nil {
			return err
		}
		return tx.Put(cleanupBucket, parentKey(parentID), parentID)
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.faults.Delete(parentID)
	run := s.active[parentID]
	if run != nil {
		run.cancel()
	}
	s.mu.Unlock()
	s.signalCleanup()
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		done, err := s.cleanupPage(ctx, parentID)
		if err != nil || done {
			return err
		}
	}
}

func (s *Scanner) cleanupPage(ctx context.Context, parentID int64) (done bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	s.mu.Lock()
	active := s.isActiveLocked(parentID)
	s.mu.Unlock()
	if active {
		return false, nil
	}
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
		if !current.ObjectsReleased {
			cleared, err := deleteCleanupPage(tx, objects, parentKey(parentID)+"/")
			if err != nil || !cleared {
				return err
			}
			current.ObjectsReleased = true
		}
		if current.Deleted {
			for _, bucket := range []string{batches, legacyKeys} {
				cleared, err := deleteCleanupPage(tx, bucket, parentKey(parentID)+"/")
				if err != nil || !cleared {
					if err == nil {
						return tx.Put(states, parentKey(parentID), current)
					}
					return err
				}
			}
			if err := tx.Delete(legacyManifest, parentKey(parentID)); err != nil {
				return err
			}
			// Keep only the fence: old requests cannot recreate a purged scan.
			current = State{Spec: Spec{ParentID: parentID, Generation: current.Generation}, Generation: current.Generation,
				Status: Stopped, Deleted: true, Purged: true, ObjectsReleased: true, UpdatedAt: time.Now().UTC()}
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

func deleteCleanupPage(tx *state.Tx, bucket, prefix string) (bool, error) {
	keys := make([]string, 0, cleanupPageKeys)
	var bytes int
	err := tx.Scan(bucket, prefix, "", cleanupPageKeys+1, func(key string, raw json.RawMessage) error {
		remaining := cleanupPageBytes - bytes
		fits := len(key) <= remaining && len(raw) <= remaining-len(key)
		if len(keys) == cleanupPageKeys || len(keys) > 0 && !fits {
			return errCleanupPageFull
		}
		keys = append(keys, key)
		if fits {
			bytes += len(key) + len(raw)
		} else {
			bytes = cleanupPageBytes // One oversized record still makes progress.
		}
		return nil
	})
	full := errors.Is(err, errCleanupPageFull)
	if err != nil && !full {
		return false, err
	}
	for _, key := range keys {
		if err := tx.Delete(bucket, key); err != nil {
			return false, err
		}
	}
	return !full, nil
}
