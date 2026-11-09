package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const s3UploadBucket = "agent-multipart-uploads"

// s3AbandonedBucket holds uploads whose writer failed to abort them, usually
// because the storage was unreachable. No writer uses them any more, so their
// aborts are retried while the Agent runs, not only at its next start.
const s3AbandonedBucket = "agent-multipart-abandoned"

// Defaults for retrying abandoned uploads' aborts: a round at most every
// abandonedRetryGap however often work starts or ends, and one every
// abandonedRetryEvery while any remain.
const (
	abandonedRetryGap   = 30 * time.Second
	abandonedRetryEvery = 5 * time.Minute
)

type S3RecoveryIssue struct {
	Record transferstore.S3UploadRecord `json:"record"`
	Error  string                       `json:"error"`
}
type S3RecoveryReport struct {
	Removed  int               `json:"removed"`
	Retained []S3RecoveryIssue `json:"retained,omitempty"`
}

// S3RecoveryRegistry keeps only upload IDs and non-secret connection identities.
// Credentials are resolved from the current saved connection during recovery.
// The exclusive Agent database lock prevents recovering a live process's uploads.
type S3RecoveryRegistry struct {
	store     *state.Store
	factory   *storageconfig.Factory
	temporary *LocalTempRegistry
	mu        sync.RWMutex
	ready     bool
	// retryMu serializes rounds of abandoned-upload aborts.
	retryMu sync.Mutex
	kick    chan struct{}
	// retryGap and retryEvery pace the background retries (see Run).
	retryGap, retryEvery time.Duration
	// rounds, when set, receives the report of each background round.
	rounds chan<- S3RecoveryReport
}

func NewS3RecoveryRegistry(store *state.Store, factory *storageconfig.Factory, temporary *LocalTempRegistry) (*S3RecoveryRegistry, error) {
	if store == nil || factory == nil || temporary == nil || temporary.store != store {
		return nil, errors.New("multipart recovery requires the same Agent database and temporary-file owner")
	}
	return &S3RecoveryRegistry{store: store, factory: factory, temporary: temporary, kick: make(chan struct{}, 1), retryGap: abandonedRetryGap, retryEvery: abandonedRetryEvery}, nil
}

func (r *S3RecoveryRegistry) OwnerID() string { return r.temporary.OwnerID() }

func (r *S3RecoveryRegistry) Recover(ctx context.Context) (S3RecoveryReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var report S3RecoveryReport
	if r.ready {
		return report, errors.New("multipart recovery is only allowed before this Agent starts I/O")
	}
	budget, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, bucket := range []string{s3UploadBucket, s3AbandonedBucket} {
		if err := r.drain(ctx, budget, bucket, &report); err != nil {
			return report, err
		}
	}
	r.ready = true
	return report, nil
}

// drain aborts every upload recorded in bucket and forgets the aborted ones.
// Uploads whose abort fails stay recorded and are listed in report.
func (r *S3RecoveryRegistry) drain(ctx, budget context.Context, bucket string, report *S3RecoveryReport) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var records []transferstore.S3UploadRecord
		if err := r.store.View(func(tx *state.Tx) error {
			return tx.Scan(bucket, "", after, 128, func(key string, raw json.RawMessage) error {
				var record transferstore.S3UploadRecord
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				if key != record.ID {
					return errors.New("multipart record key mismatch")
				}
				records = append(records, record)
				after = key
				return nil
			})
		}); err != nil {
			return err
		}
		var cleared []string
		for _, record := range records {
			err := r.abort(budget, record)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				report.Retained = append(report.Retained, S3RecoveryIssue{Record: record, Error: err.Error()})
				continue
			}
			cleared = append(cleared, record.ID)
		}
		if len(cleared) > 0 {
			if err := r.store.Update(func(tx *state.Tx) error {
				for _, id := range cleared {
					if err := tx.Delete(bucket, id); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			report.Removed += len(cleared)
		}
		if len(records) < 128 {
			return nil
		}
	}
}

func (r *S3RecoveryRegistry) abort(ctx context.Context, record transferstore.S3UploadRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Owner != r.OwnerID() {
		return errors.New("multipart upload belongs to another database owner")
	}
	if err := transferstore.ValidateS3UploadRecord(record); err != nil {
		return err
	}
	request, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	store, err := r.factory.OpenEndpoint(request, execution.Endpoint{Kind: "S3", Ref: record.Connection.Alias, Bucket: record.Bucket})
	if err != nil {
		return err
	}
	defer store.Close()
	s3, ok := store.(*transferstore.S3)
	if !ok {
		return errors.New("multipart recovery requires an S3 client")
	}
	return s3.AbortRecordedUpload(request, record)
}

func (r *S3RecoveryRegistry) RegisterUpload(ctx context.Context, record transferstore.S3UploadRecord) error {
	if err := transferstore.ValidateS3UploadRecord(record); err != nil {
		return err
	}
	if record.Owner != r.OwnerID() {
		return errors.New("multipart upload belongs to another database owner")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready {
		return errors.New("multipart recovery has not completed")
	}
	return r.temporary.submit(ctx, func(tx *state.Tx) error {
		var old transferstore.S3UploadRecord
		found, err := tx.Get(s3UploadBucket, record.ID, &old)
		if err != nil {
			return err
		}
		if found && old != record {
			return errors.New("multipart record ID collision")
		}
		return tx.Put(s3UploadBucket, record.ID, record)
	})
}

func (r *S3RecoveryRegistry) ForgetUpload(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.temporary.submit(ctx, func(tx *state.Tx) error { return tx.Delete(s3UploadBucket, id) })
}

// AbandonUpload moves an upload whose writer could not abort it to the
// abandoned uploads, whose aborts RetryAbandoned retries. The upload's
// record stays durable throughout.
func (r *S3RecoveryRegistry) AbandonUpload(ctx context.Context, record transferstore.S3UploadRecord) error {
	if err := transferstore.ValidateS3UploadRecord(record); err != nil {
		return err
	}
	if record.Owner != r.OwnerID() {
		return errors.New("multipart upload belongs to another database owner")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready {
		return errors.New("multipart recovery has not completed")
	}
	return r.temporary.submit(ctx, func(tx *state.Tx) error {
		var live transferstore.S3UploadRecord
		found, err := tx.Get(s3UploadBucket, record.ID, &live)
		if err != nil {
			return err
		}
		if !found {
			return nil // Already aborted or forgotten.
		}
		if live != record {
			return errors.New("multipart record ID collision")
		}
		if err := tx.Delete(s3UploadBucket, record.ID); err != nil {
			return err
		}
		return tx.Put(s3AbandonedBucket, record.ID, record)
	})
}

// abandoned reports whether any upload's abort is still to be retried.
func (r *S3RecoveryRegistry) abandoned() (bool, error) {
	found := false
	err := r.store.View(func(tx *state.Tx) error {
		return tx.ScanKeys(s3AbandonedBucket, "", "", 1, func(string, int64) error {
			found = true
			return nil
		})
	})
	return found, err
}

// RetryAbandoned aborts the abandoned uploads once more and forgets those
// that are gone. It may run while transfers continue: abandoned uploads are
// no longer used by any writer.
func (r *S3RecoveryRegistry) RetryAbandoned(ctx context.Context) (S3RecoveryReport, error) {
	r.mu.RLock()
	ready := r.ready
	r.mu.RUnlock()
	var report S3RecoveryReport
	if !ready {
		return report, errors.New("multipart recovery has not completed")
	}
	r.retryMu.Lock()
	defer r.retryMu.Unlock()
	budget, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := r.drain(ctx, budget, s3AbandonedBucket, &report)
	return report, err
}

// Kick asks Run for a round soon, for example when a task starts, resumes,
// retries or ends. Kicks are coalesced and paced.
func (r *S3RecoveryRegistry) Kick() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run retries abandoned uploads' aborts until ctx ends: after a Kick, and
// periodically while any remain. Rounds are at least retryGap apart, so a
// burst of tasks starting or ending does not repeat them.
func (r *S3RecoveryRegistry) Run(ctx context.Context) {
	timer := time.NewTimer(r.retryEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-timer.C:
		}
		if found, err := r.abandoned(); err == nil && found {
			report, _ := r.RetryAbandoned(ctx)
			if r.rounds != nil {
				select {
				case r.rounds <- report:
				case <-ctx.Done():
					return
				}
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.retryEvery)
		gap := time.NewTimer(r.retryGap)
		select {
		case <-ctx.Done():
			gap.Stop()
			return
		case <-gap.C:
		}
	}
}

func (r *S3RecoveryRegistry) Factory(base execution.StoreFactory) execution.StoreFactory {
	return execution.StoreFactoryFunc(func(ctx context.Context, job execution.Job) (transferstore.Store, transferstore.Store, error) {
		r.mu.RLock()
		ready := r.ready
		r.mu.RUnlock()
		if !ready || base == nil {
			return nil, nil, errors.New("multipart recovery has not completed")
		}
		source, destination, err := base.Open(ctx, job)
		if err != nil {
			return nil, nil, err
		}
		if job.Destination.Kind == "S3" {
			// A task starting, resuming or retrying is a good time to clean
			// up uploads an earlier failure could not abort.
			r.Kick()
			s3, ok := destination.(*transferstore.S3)
			if !ok {
				err = errors.New("S3 destination cannot track multipart recovery")
			} else {
				err = s3.SetRecoveryRegistry(r, job.Destination.Ref, r.temporary)
			}
			if err != nil {
				source.Close()
				destination.Close()
				return nil, nil, err
			}
		}
		return source, destination, nil
	})
}
