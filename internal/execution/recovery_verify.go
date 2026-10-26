package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Revalidate every credited object before resuming. These checks use the same
// adjustable object slots as transfers, so recovery cannot flood a store with
// an unbounded burst of HEAD requests.
func (e *Engine) verifyCompleted(parent context.Context, record Record, attempt *activeAttempt, target transferstore.Store) ([]string, error) {
	if len(record.Results) == 0 {
		return nil, parent.Err()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type checked struct {
		invalid string
		err     error
	}
	results := make(chan checked, e.opts.QueueSize)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for _, result := range record.Results {
			if result.Status != ItemSucceeded && result.Status != ItemSkipped {
				continue
			}
			if err := e.acquireObject(ctx, attempt); err != nil {
				select {
				case results <- checked{err: err}:
				case <-ctx.Done():
				}
				return
			}
			workers.Add(1)
			go func(result ItemResult) {
				defer workers.Done()
				defer e.releaseObject(attempt)
				invalid, err := verifyCompletedObjectForJob(ctx, target, result, record.Job)
				out := checked{invalid: invalid, err: err}
				select {
				case results <- out:
				case <-ctx.Done():
				}
			}(result)
		}
	}()
	go func() { workers.Wait(); close(results) }()
	var invalid []string
	var firstErr error
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			cancel()
		}
		if result.invalid != "" {
			invalid = append(invalid, result.invalid)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return invalid, nil
}

func verifyCompletedObject(ctx context.Context, target transferstore.Store, result ItemResult) (invalid string, err error) {
	return verifyCompletedObjectForJob(ctx, target, result, Job{})
}

func verifyCompletedObjectForJob(ctx context.Context, target transferstore.Store, result ItemResult, job Job) (invalid string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("verify completed target %q: panic: %v", result.Target.Key, recovered)
		}
	}()
	actual, err := target.Stat(ctx, result.Target.Key)
	if errors.Is(err, transferstore.ErrNotFound) {
		return result.ID, nil
	}
	if err != nil {
		return "", fmt.Errorf("verify completed target %q: %w", result.Target.Key, err)
	}
	expected := result.Target
	if job.SharedLocalDestination && job.Destination.Kind == "LOCAL" {
		if !strings.HasPrefix(expected.ETag, "local:") || expected.ModifiedAt.IsZero() || expected.VersionID != "" {
			return "", errors.New("shared local target requires a complete local result fingerprint")
		}
		// The Agent has proved this is the same shared destination. Device and
		// inode numbers may differ across mounts; size and mtime remain checked.
		expected.ETag = ""
	}
	if changed(actual, expected) {
		return result.ID, nil
	}
	return "", nil
}
