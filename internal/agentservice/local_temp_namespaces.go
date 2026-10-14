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
	if found, err := read(); err != nil || found {
		return record, err
	}
	// First use is serialized; subsequent writes only read the committed record.
	r.namespaceMu.Lock()
	defer r.namespaceMu.Unlock()
	if found, err := read(); err != nil || found {
		return record, err
	}
	var err error
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
