package agentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const localTempNamespacesBucket = "agent-local-temp-namespaces"

func namespaceKey(parent string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(parent)))
	return hex.EncodeToString(digest[:])
}

// PrepareNamespace commits the directory identity before Local creates any
// payload file. Existing namespaces are never replaced or adopted by pathname.
// Each successful call holds a reference until ReleaseNamespace.
func (r *LocalTempRegistry) PrepareNamespace(ctx context.Context, parent *os.Root, parentPath string) (transferstore.LocalTempNamespace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var record transferstore.LocalTempNamespace
	if r.closed || !r.ready {
		return record, errors.New("temporary-file registry is not ready")
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	key := namespaceKey(parentPath)
	// Serialized with removal. Local calls this once per directory per job.
	r.namespaceMu.Lock()
	defer r.namespaceMu.Unlock()
	found, err := r.namespaceRecord(key, &record)
	if err == nil && found && (record.Owner != r.owner || filepath.Clean(record.ParentPath) != filepath.Clean(parentPath)) {
		err = transferstore.ErrTempIdentity
	}
	if err != nil {
		return record, err
	}
	delete(r.idle, key)
	if found && r.namespaces[key] == 0 {
		// Nobody holds it, so a failed removal may have left it unusable.
		// Finish that removal and start over instead of handing it out.
		if check, openErr := transferstore.OpenLocalTempNamespace(parent, record); openErr == nil {
			check.Close()
		} else {
			removed, removeErr := r.removeNamespace(ctx, key, record)
			if !removed {
				return record, errors.Join(openErr, removeErr)
			}
			found = false
		}
	}
	if !found {
		record, err = transferstore.CreateLocalTempNamespace(ctx, parent, parentPath, r.owner)
		if err != nil {
			return record, err
		}
		if err := r.store.Update(func(tx *state.Tx) error { return tx.Put(localTempNamespacesBucket, key, record) }); err != nil {
			return record, err
		}
	}
	r.namespaces[key]++
	return record, nil
}

func (r *LocalTempRegistry) namespaceRecord(key string, record *transferstore.LocalTempNamespace) (bool, error) {
	var found bool
	err := r.store.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get(localTempNamespacesBucket, key, record)
		return err
	})
	if err == nil && found {
		err = transferstore.ValidateLocalTempNamespace(*record)
	}
	return found, err
}

// ReleaseNamespace drops one PrepareNamespace reference. After the last
// release the namespace stays idle for namespaceIdleGrace, so consecutive jobs
// writing into the same directory reuse it, and is then removed with its
// record, so finished transfers do not leave hidden directories behind.
func (r *LocalTempRegistry) ReleaseNamespace(ctx context.Context, record transferstore.LocalTempNamespace) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := namespaceKey(record.ParentPath)
	r.namespaceMu.Lock()
	defer r.namespaceMu.Unlock()
	if r.namespaces[key] <= 0 {
		return errors.New("temporary namespace released more often than prepared")
	}
	if r.namespaces[key]--; r.namespaces[key] > 0 {
		return nil
	}
	delete(r.namespaces, key)
	if r.closed {
		return nil // Startup recovery removes it.
	}
	r.idle[key] = idleNamespace{record: record, since: time.Now()}
	return nil
}

// namespaceIdleGrace is how long a released namespace waits for reuse.
const namespaceIdleGrace = 15 * time.Second

type idleNamespace struct {
	record transferstore.LocalTempNamespace
	since  time.Time
}

func (r *LocalTempRegistry) sweepLoop() {
	ticker := time.NewTicker(namespaceIdleGrace / 3)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopSweep:
			return
		case <-ticker.C:
			r.mu.RLock()
			if !r.closed {
				r.sweepIdleNamespaces(context.Background(), time.Now().Add(-namespaceIdleGrace))
			}
			r.mu.RUnlock()
		}
	}
}

// sweepIdleNamespaces removes namespaces idle since before cutoff; a zero
// cutoff removes all of them. A namespace that cannot be removed now is left
// for restart recovery.
func (r *LocalTempRegistry) sweepIdleNamespaces(ctx context.Context, cutoff time.Time) {
	r.namespaceMu.Lock()
	defer r.namespaceMu.Unlock()
	for key, idle := range r.idle {
		if !cutoff.IsZero() && idle.since.After(cutoff) {
			continue
		}
		delete(r.idle, key)
		_, _ = r.removeNamespace(ctx, key, idle.record)
	}
}

// removeNamespace runs with no live reference: during recovery, or under
// namespaceMu after the last release. A namespace that still has temporary-file
// records is kept, because recovery opens those files through it.
func (r *LocalTempRegistry) removeNamespace(ctx context.Context, key string, record transferstore.LocalTempNamespace) (bool, error) {
	var stored transferstore.LocalTempNamespace
	found, err := r.namespaceRecord(key, &stored)
	if err != nil || !found || stored != record {
		return false, err
	}
	directory := filepath.Join(record.ParentPath, record.Name)
	tracked := false
	if err := r.store.View(func(tx *state.Tx) error {
		return tx.Scan(localTemporaryBucket, "", "", math.MaxInt, func(_ string, raw json.RawMessage) error {
			var temp transferstore.LocalTempRecord
			if err := json.Unmarshal(raw, &temp); err != nil {
				return err
			}
			if filepath.Clean(temp.ParentPath) == directory {
				tracked = true
				return errStopScan
			}
			return nil
		})
	}); err != nil && !errors.Is(err, errStopScan) {
		return false, err
	}
	if tracked {
		return false, nil
	}
	// Delete the directory before its record: an interruption in between is
	// finished by the next recovery, which accepts a missing directory.
	if err := transferstore.RemoveLocalTempNamespace(ctx, record); err != nil {
		return false, err
	}
	err = r.store.Update(func(tx *state.Tx) error { return tx.Delete(localTempNamespacesBucket, key) })
	return err == nil, err
}

var errStopScan = errors.New("stop scan")

func (r *LocalTempRegistry) recoverNamespaces(ctx context.Context, registered map[string]bool, report *TempRecoveryReport) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var records []transferstore.LocalTempNamespace
		if err := r.store.View(func(tx *state.Tx) error {
			return tx.Scan(localTempNamespacesBucket, "", after, 128, func(key string, raw json.RawMessage) error {
				var record transferstore.LocalTempNamespace
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				if key != namespaceKey(record.ParentPath) {
					return errors.New("temporary namespace record key mismatch")
				}
				records = append(records, record)
				after = key
				return nil
			})
		}); err != nil {
			return err
		}
		for _, record := range records {
			var count int
			var err error
			if record.Owner != r.owner {
				err = errors.New("temporary namespace belongs to another database owner")
			} else {
				count, err = transferstore.RemoveUnregisteredNamespaceTemps(ctx, record, registered)
			}
			report.Removed += count
			if record.Owner == r.owner {
				// Recovery runs before any I/O, so no namespace is in use. This
				// also finishes a removal interrupted after deleting the marker
				// or the directory, which the sweep above reports as an error.
				removed, removeErr := r.removeNamespace(ctx, namespaceKey(record.ParentPath), record)
				switch {
				case removed:
					report.RemovedNamespaces++
					err = nil
				case err == nil && !errors.Is(removeErr, transferstore.ErrTempNamespaceInUse):
					err = removeErr
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				report.Retained = append(report.Retained, TempRecoveryIssue{Namespace: &record, Error: err.Error()})
			}
		}
		if len(records) < 128 {
			return nil
		}
	}
}
