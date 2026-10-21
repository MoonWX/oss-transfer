package agentservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const localTemporaryBucket = "agent-local-temporary"

type TempRecoveryIssue struct {
	Record    transferstore.LocalTempRecord     `json:"record"`
	Error     string                            `json:"error"`
	Namespace *transferstore.LocalTempNamespace `json:"namespace,omitempty"`
}
type TempRecoveryReport struct {
	Removed  int                 `json:"removed"`
	Missing  int                 `json:"missing"`
	Retained []TempRecoveryIssue `json:"retained,omitempty"`
}
type recoveryMutation struct {
	apply func(*state.Tx) error
	done  chan error
}

// LocalTempRegistry belongs to one exclusively opened Agent database. It never
// scans arbitrary storage directories: recovery visits this owner's recorded
// files and private, durably registered staging namespaces.
// Call Recover before handing its Factory to Engine, then Close after Engine.
type LocalTempRegistry struct {
	store       *state.Store
	owner       string
	mu          sync.RWMutex
	namespaceMu sync.Mutex
	ready       bool
	closed      bool
	queue       chan recoveryMutation
	done        chan struct{}
}

func NewLocalTempRegistry(store *state.Store) (*LocalTempRegistry, error) {
	if store == nil {
		return nil, errors.New("Agent state database is required")
	}
	if err := store.ClaimRole("agent"); err != nil {
		return nil, err
	}
	var owner string
	if err := store.Update(func(tx *state.Tx) error {
		found, err := tx.Get("metadata", "localTemporaryOwner", &owner)
		if err != nil {
			return err
		}
		if found {
			decoded, err := hex.DecodeString(owner)
			if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != owner {
				return errors.New("invalid temporary-file database owner")
			}
			return nil
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		owner = hex.EncodeToString(random[:])
		return tx.Put("metadata", "localTemporaryOwner", owner)
	}); err != nil {
		return nil, err
	}
	r := &LocalTempRegistry{store: store, owner: owner, queue: make(chan recoveryMutation, 256), done: make(chan struct{})}
	go r.commitLoop()
	return r, nil
}

func (r *LocalTempRegistry) OwnerID() string { return r.owner }

func (r *LocalTempRegistry) Recover(ctx context.Context) (TempRecoveryReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var report TempRecoveryReport
	if r.ready || r.closed {
		return report, errors.New("temporary recovery is only allowed before this Agent starts I/O")
	}
	registered := make(map[string]bool)
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		var records []transferstore.LocalTempRecord
		if err := r.store.View(func(tx *state.Tx) error {
			return tx.Scan(localTemporaryBucket, "", after, 128, func(key string, raw json.RawMessage) error {
				var record transferstore.LocalTempRecord
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				if key != record.ID {
					return errors.New("temporary-file record key mismatch")
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
			registered[filepath.Join(record.ParentPath, record.Name)] = true
			var removed bool
			var err error
			if record.Owner != r.owner {
				err = errors.New("temporary file belongs to a different database owner")
			} else {
				removed, err = transferstore.RemoveTrackedLocalTemp(ctx, record)
			}
			if err != nil {
				if ctx.Err() != nil {
					return report, ctx.Err()
				}
				report.Retained = append(report.Retained, TempRecoveryIssue{Record: record, Error: err.Error()})
				continue
			}
			cleared = append(cleared, record.ID)
			if removed {
				report.Removed++
			} else {
				report.Missing++
			}
		}
		if len(cleared) > 0 {
			if err := r.store.Update(func(tx *state.Tx) error {
				for _, id := range cleared {
					if err := tx.Delete(localTemporaryBucket, id); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return report, err
			}
		}
		if len(records) < 128 {
			break
		}
	}
	if err := r.recoverNamespaces(ctx, registered, &report); err != nil {
		return report, err
	}
	r.ready = true
	return report, nil
}

func (r *LocalTempRegistry) Register(ctx context.Context, record transferstore.LocalTempRecord) error {
	if record.Owner != r.owner {
		return errors.New("temporary file belongs to another database owner")
	}
	if err := transferstore.ValidateLocalTempRecord(record); err != nil {
		return err
	}
	return r.submit(ctx, func(tx *state.Tx) error {
		var old transferstore.LocalTempRecord
		found, err := tx.Get(localTemporaryBucket, record.ID, &old)
		if err != nil {
			return err
		}
		if found && old != record {
			return errors.New("temporary-file ID collision")
		}
		return tx.Put(localTemporaryBucket, record.ID, record)
	})
}

func (r *LocalTempRegistry) Forget(ctx context.Context, id string) error {
	return r.submit(ctx, func(tx *state.Tx) error { return tx.Delete(localTemporaryBucket, id) })
}

func (r *LocalTempRegistry) submit(ctx context.Context, apply func(*state.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mutation := recoveryMutation{apply: apply, done: make(chan error, 1)}
	r.mu.RLock()
	if r.closed || !r.ready {
		r.mu.RUnlock()
		return errors.New("temporary-file registry is not ready")
	}
	select {
	case r.queue <- mutation:
		r.mu.RUnlock()
	case <-ctx.Done():
		r.mu.RUnlock()
		return ctx.Err()
	}
	// After enqueueing, the caller must know whether registration committed.
	// Returning early on cancellation could commit a record after its file was
	// cleaned, leaving a journal entry that normal cancellation cannot clear.
	return <-mutation.done
}

// Group local-file and S3 multipart recovery records in one fsynced transaction.
// Every caller waits for commit, including callers cancelled after enqueueing;
// payload I/O cannot pass its registration barrier early. A failed batch is
// rolled back and fails every caller without replaying any callback.
func (r *LocalTempRegistry) commitLoop() {
	defer close(r.done)
	for first := range r.queue {
		batch := []recoveryMutation{first}
		timer := time.NewTimer(2 * time.Millisecond)
		closed := false
	collect:
		for len(batch) < 128 {
			select {
			case next, ok := <-r.queue:
				if !ok {
					closed = true
					break collect
				}
				batch = append(batch, next)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		err := r.store.Update(func(tx *state.Tx) error {
			for _, mutation := range batch {
				if err := mutation.apply(tx); err != nil {
					return err
				}
			}
			return nil
		})
		for _, mutation := range batch {
			mutation.done <- err
		}
		if closed {
			return
		}
	}
}

func (r *LocalTempRegistry) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
}

func (r *LocalTempRegistry) Factory(base execution.StoreFactory) execution.StoreFactory {
	return execution.StoreFactoryFunc(func(ctx context.Context, job execution.Job) (transferstore.Store, transferstore.Store, error) {
		r.mu.RLock()
		ready := r.ready && !r.closed
		r.mu.RUnlock()
		if !ready {
			return nil, nil, errors.New("temporary recovery has not completed")
		}
		if base == nil {
			return nil, nil, errors.New("storage factory is required")
		}
		source, destination, err := base.Open(ctx, job)
		if err != nil {
			return nil, nil, err
		}
		if job.Destination.Kind == "LOCAL" {
			local, ok := destination.(*transferstore.Local)
			if !ok {
				source.Close()
				destination.Close()
				return nil, nil, fmt.Errorf("LOCAL destination does not support durable temporary-file tracking")
			}
			local.SetTempRegistry(r)
		}
		return source, destination, nil
	})
}
