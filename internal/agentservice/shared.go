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
}

func NewSharedVerifier() *SharedVerifier {
	return &SharedVerifier{verified: map[sharedVerificationKey]bool{}, highest: map[int64]uint64{}}
}

func (s *SharedVerifier) Verify(ctx context.Context, parentID int64, generation uint64, root string, probe transferstore.SharedProbe) error {
	if parentID < 1 || generation == 0 || !filepath.IsAbs(root) {
		return errors.New("shared storage verification requires parent, generation and absolute root")
	}
	key := sharedVerificationKey{parentID, generation, filepath.Clean(root)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verified == nil {
		s.verified = map[sharedVerificationKey]bool{}
		s.highest = map[int64]uint64{}
	}
	if generation < s.highest[parentID] {
		return ErrSharedVerificationStale
	}
	if generation > s.highest[parentID] {
		for prior := range s.verified {
			if prior.parentID == parentID {
				delete(s.verified, prior)
			}
		}
		s.highest[parentID] = generation
	}
	delete(s.verified, key)
	if err := transferstore.VerifySharedProbe(ctx, key.root, probe); err != nil {
		return err
	}
	s.verified[key] = true
	return nil
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
