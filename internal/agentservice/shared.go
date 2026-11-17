package agentservice

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

var ErrSharedVerificationStale = errors.New("shared storage verification generation is stale")
var ErrSharedVerificationRequired = errors.New("shared storage verification is required for this Agent boot and task generation")

type sharedVerificationKey struct {
	parentID   int64
	generation uint64
	root       string
}

// SharedVerifier holds proofs for this Agent process only. Construct a fresh
// verifier on boot; recovered shared-local jobs must wait for Master revalidation.
// Paths must first pass the same Agent root authorization as execution endpoints.
type SharedVerifier struct {
	mu       sync.RWMutex
	verified map[sharedVerificationKey]bool
	highest  map[int64]uint64
	// fingerprints spares rereading unchanged files: the Master re-checks a
	// shared source that no node can write on every task pass.
	fingerprints transferstore.SharedFingerprintMemo
}

func NewSharedVerifier() *SharedVerifier {
	return &SharedVerifier{verified: map[sharedVerificationKey]bool{}, highest: map[int64]uint64{}}
}

func (s *SharedVerifier) Verify(ctx context.Context, parentID int64, generation uint64, root string, probe transferstore.SharedProbe) error {
	_, err := s.prove(parentID, generation, root, func(root string) error { return transferstore.VerifySharedProbe(ctx, root, probe) })
	return err
}

// VerifyFingerprints records a proof from sampled file fingerprints, used
// when no Agent can write the probe file into a shared upload source.
func (s *SharedVerifier) VerifyFingerprints(ctx context.Context, parentID int64, generation uint64, root string, fingerprints []transferstore.SharedFingerprint) error {
	_, err := s.prove(parentID, generation, root, func(root string) error {
		return s.fingerprints.Verify(ctx, root, fingerprints)
	})
	return err
}

// Skip records a proof without any check. The Master sends it only for a task
// whose operator explicitly acknowledged skipping the shared-directory check;
// the generation fence still applies, so a stopped task cannot reuse it.
// Granted reports that this process held no such proof before.
func (s *SharedVerifier) Skip(parentID int64, generation uint64, root string) (granted bool, err error) {
	held, err := s.prove(parentID, generation, root, func(string) error { return nil })
	return err == nil && !held, err
}

// prove replaces the proof for one task generation and root with the result
// of check. held reports whether the proof existed before.
func (s *SharedVerifier) prove(parentID int64, generation uint64, root string, check func(root string) error) (held bool, err error) {
	if parentID < 1 || generation == 0 || !filepath.IsAbs(root) {
		return false, errors.New("shared storage verification requires parent, generation and absolute root")
	}
	key := sharedVerificationKey{parentID, generation, filepath.Clean(root)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verified == nil {
		s.verified = map[sharedVerificationKey]bool{}
		s.highest = map[int64]uint64{}
	}
	if generation < s.highest[parentID] {
		return false, ErrSharedVerificationStale
	}
	if generation > s.highest[parentID] {
		for prior := range s.verified {
			if prior.parentID == parentID {
				delete(s.verified, prior)
			}
		}
		s.highest[parentID] = generation
	}
	held = s.verified[key]
	delete(s.verified, key)
	if err := check(key.root); err != nil {
		return held, err
	}
	s.verified[key] = true
	return held, nil
}

func (s *SharedVerifier) Verified(parentID int64, generation uint64, root string) bool {
	if s == nil || parentID < 1 || generation == 0 || !filepath.IsAbs(root) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.verified[sharedVerificationKey{parentID, generation, filepath.Clean(root)}]
}

// Forget revokes matching proofs after stop/delete; generation zero revokes all
// generations. Retain the generation fence so old verification cannot supersede it.
func (s *SharedVerifier) Forget(parentID int64, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.verified {
		if key.parentID == parentID && (generation == 0 || key.generation == generation) {
			delete(s.verified, key)
		}
	}
}

func (s *SharedVerifier) Allows(job execution.Job) bool {
	if job.SharedLocalSource && (job.Source.Kind != "LOCAL" || !s.Verified(job.ParentID, job.Generation, job.Source.Root)) {
		return false
	}
	if job.SharedLocalDestination && (job.Destination.Kind != "LOCAL" || !s.Verified(job.ParentID, job.Generation, job.Destination.Root)) {
		return false
	}
	return true
}

// Factory adds a second guard at actual I/O startup, after scheduler admission.
// A proof revoked while a job waited for a slot must not open source objects.
func (s *SharedVerifier) Factory(base execution.StoreFactory) execution.StoreFactory {
	return execution.StoreFactoryFunc(func(ctx context.Context, job execution.Job) (transferstore.Store, transferstore.Store, error) {
		if !s.Allows(job) {
			return nil, nil, ErrSharedVerificationRequired
		}
		if base == nil {
			return nil, nil, errors.New("storage factory is required")
		}
		return base.Open(ctx, job)
	})
}
