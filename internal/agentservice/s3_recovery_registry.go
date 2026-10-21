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
}

func NewS3RecoveryRegistry(store *state.Store, factory *storageconfig.Factory, temporary *LocalTempRegistry) (*S3RecoveryRegistry, error) {
	if store == nil || factory == nil || temporary == nil || temporary.store != store {
		return nil, errors.New("multipart recovery requires the same Agent database and temporary-file owner")
	}
	return &S3RecoveryRegistry{store: store, factory: factory, temporary: temporary}, nil
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
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		var records []transferstore.S3UploadRecord
		if err := r.store.View(func(tx *state.Tx) error {
			return tx.Scan(s3UploadBucket, "", after, 128, func(key string, raw json.RawMessage) error {
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
			return report, err
		}
		var cleared []string
		for _, record := range records {
			err := r.abort(budget, record)
			if ctx.Err() != nil {
				return report, ctx.Err()
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
					if err := tx.Delete(s3UploadBucket, id); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return report, err
			}
			report.Removed += len(cleared)
		}
		if len(records) < 128 {
			break
		}
	}
	r.ready = true
	return report, nil
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
