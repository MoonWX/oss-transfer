package agentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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
// The namespace stays in use until ReleaseNamespace.
func (r *LocalTempRegistry) PrepareNamespace(ctx context.Context, parent *os.Root, parentPath string) (transferstore.LocalTempNamespace, error) {
	key := namespaceKey(parentPath)
	r.useMu.Lock()
	r.inUse[key]++
	r.used[key] = true
	r.useMu.Unlock()
	record, err := r.prepareNamespace(ctx, parent, parentPath)
	if err != nil {
		r.release(key)
	}
	return record, err
}

// ReleaseNamespace ends one use of a namespace PrepareNamespace returned.
func (r *LocalTempRegistry) ReleaseNamespace(record transferstore.LocalTempNamespace) {
	r.release(namespaceKey(record.ParentPath))
}

func (r *LocalTempRegistry) release(key string) {
	r.useMu.Lock()
	defer r.useMu.Unlock()
	if r.inUse[key]--; r.inUse[key] <= 0 {
		delete(r.inUse, key)
	}
}

// SweepNamespaces removes the staging namespaces used since the last sweep
// that no writer uses now and that hold nothing but their marker, so a
// finished download leaves no staging directory beside its files. A
// namespace still in use, or holding a retained temporary file, is kept and
// considered again by a later sweep.
func (r *LocalTempRegistry) SweepNamespaces(ctx context.Context) (int, error) {
	r.useMu.Lock()
	keys := make([]string, 0, len(r.used))
	for key := range r.used {
		keys = append(keys, key)
	}
	r.used = map[string]bool{}
	r.useMu.Unlock()
	removed := 0
	var errs error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		gone, err := r.sweepNamespace(ctx, key)
		if gone {
			removed++
		}
		errs = errors.Join(errs, err)
	}
	return removed, errs
}

func (r *LocalTempRegistry) sweepNamespace(ctx context.Context, key string) (bool, error) {
	r.useMu.Lock()
	defer r.useMu.Unlock()
	if r.inUse[key] > 0 {
		r.used[key] = true
		return false, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || !r.ready {
		return false, nil
	}
	var record transferstore.LocalTempNamespace
	var found bool
	if err := r.store.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get(localTempNamespacesBucket, key, &record)
		return err
	}); err != nil || !found {
		return false, err
	}
	if record.Owner != r.owner {
		return false, transferstore.ErrTempIdentity
	}
	gone, err := transferstore.RemoveEmptyLocalTempNamespace(ctx, record)
	if !gone {
		return false, err
	}
	return true, r.store.Update(func(tx *state.Tx) error { return tx.Delete(localTempNamespacesBucket, key) })
}

func (r *LocalTempRegistry) prepareNamespace(ctx context.Context, parent *os.Root, parentPath string) (transferstore.LocalTempNamespace, error) {
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
	read := func() (bool, error) {
		var found bool
		err := r.store.View(func(tx *state.Tx) error {
			var err error
			found, err = tx.Get(localTempNamespacesBucket, key, &record)
			return err
		})
		if err == nil && found {
			if record.Owner != r.owner || filepath.Clean(record.ParentPath) != filepath.Clean(parentPath) {
				return false, transferstore.ErrTempIdentity
			}
			err = transferstore.ValidateLocalTempNamespace(record)
		}
		return found, err
	}
	found, err := read()
	if err != nil {
		return record, err
	}
	if found {
		gone, goneErr := transferstore.LocalTempNamespaceGone(parent, record)
		if goneErr != nil || !gone {
			return record, goneErr
		}
	}
	// First use, or replacing a namespace whose directory was deleted (for
	// example when the target directory was removed and recreated), is
	// serialized; subsequent writes only read the committed record.
	r.namespaceMu.Lock()
	defer r.namespaceMu.Unlock()
	if found, err = read(); err != nil {
		return record, err
	}
	if found {
		gone, goneErr := transferstore.LocalTempNamespaceGone(parent, record)
		if goneErr != nil || !gone {
			return record, goneErr
		}
	}
	record, err = transferstore.CreateLocalTempNamespace(ctx, parent, parentPath, r.owner)
	if err != nil {
		return record, err
	}
	err = r.store.Update(func(tx *state.Tx) error { return tx.Put(localTempNamespacesBucket, key, record) })
	return record, err
}

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
				if err == nil {
					// Nothing runs yet: an empty namespace is a leftover
					// staging directory.
					var gone bool
					if gone, err = transferstore.RemoveEmptyLocalTempNamespace(ctx, record); gone {
						err = errors.Join(err, r.store.Update(func(tx *state.Tx) error { return tx.Delete(localTempNamespacesBucket, namespaceKey(record.ParentPath)) }))
					}
				}
			}
			report.Removed += count
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
