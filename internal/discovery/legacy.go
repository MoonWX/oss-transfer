package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const legacyManifest = "discovery-legacy-manifest"
const legacyKeys = "discovery-legacy-keys"

type LegacyKey struct {
	Key          string `json:"key"`
	Size         *int64 `json:"size,omitempty"`
	Successful   bool   `json:"successful,omitempty"`
	TimeFiltered bool   `json:"timeFiltered,omitempty"`
}
type LegacyManifest struct {
	MigrationID string `json:"migrationId"`
	ParentID    int64  `json:"parentId"`
	Exact       bool   `json:"exact"`
	Sealed      bool   `json:"sealed"`
	Count       int64  `json:"count"`
}

// ImportLegacyKeys is an offline import primitive. Each bounded batch commits
// atomically; duplicate keys merge without increasing the manifest count.
func ImportLegacyKeys(store *state.Store, manifest LegacyManifest, rows []LegacyKey) error {
	if manifest.ParentID < 1 || manifest.MigrationID == "" || len(rows) > 128 {
		return ErrInvalid
	}
	return store.Update(func(tx *state.Tx) error {
		var guard struct {
			Pending     bool
			MigrationID string
		}
		found, err := tx.Get("migration", "state", &guard)
		if err != nil {
			return err
		}
		if !found || !guard.Pending || guard.MigrationID != manifest.MigrationID {
			return errors.New("legacy manifest import requires pending migration")
		}
		var current LegacyManifest
		found, err = tx.Get(legacyManifest, parentKey(manifest.ParentID), &current)
		if err != nil {
			return err
		}
		if found && (current.MigrationID != manifest.MigrationID || current.Exact != manifest.Exact) {
			return ErrConflict
		}
		if !found {
			current = manifest
			current.Sealed = false
			current.Count = 0
		}
		for _, row := range rows {
			if row.Key == "" || len(row.Key) > 1024 || !utf8.ValidString(row.Key) || strings.ContainsRune(row.Key, '\x00') || row.Size != nil && *row.Size < 0 {
				return ErrInvalid
			}
			k := objectKey(manifest.ParentID, row.Key)
			var old LegacyKey
			found, err := tx.Get(legacyKeys, k, &old)
			if err != nil {
				return err
			}
			if found {
				if current.Sealed {
					if row.Successful && !old.Successful || row.TimeFiltered && !old.TimeFiltered {
						return ErrConflict
					}
					continue
				}
				row.Successful = row.Successful || old.Successful
				row.TimeFiltered = row.TimeFiltered || old.TimeFiltered
				if old.Size == nil || row.Size == nil || *old.Size != *row.Size {
					row.Size = nil
				}
			} else {
				if current.Sealed {
					return ErrConflict
				}
				current.Count++
			}
			if err := tx.Put(legacyKeys, k, row); err != nil {
				return err
			}
		}
		if manifest.Sealed {
			current.Sealed = true
		}
		return tx.Put(legacyManifest, parentKey(manifest.ParentID), current)
	})
}
func LegacyManifestStatus(store *state.Store, parentID int64) (manifest LegacyManifest, err error) {
	err = store.View(func(tx *state.Tx) error {
		found, e := tx.Get(legacyManifest, parentKey(parentID), &manifest)
		if e == nil && !found {
			return ErrNotFound
		}
		return e
	})
	return
}
func (s *Scanner) legacyCandidate(tx *state.Tx, spec Spec, object transferstore.Object, missing bool) (*execution.LegacyCandidate, error) {
	if spec.LegacyManifestID == "" {
		return nil, nil
	}
	var key LegacyKey
	found, err := tx.Get(legacyKeys, objectKey(spec.ParentID, object.Key), &key)
	if err != nil || !found {
		return nil, err
	}
	return &execution.LegacyCandidate{Successful: key.Successful, Missing: missing, SourceChanged: key.Size != nil && *key.Size != object.Size}, nil
}
func (s *Scanner) legacyHeader(spec Spec) (LegacyManifest, error) {
	manifest, err := LegacyManifestStatus(s.store, spec.ParentID)
	if err != nil {
		return manifest, err
	}
	if !manifest.Sealed || manifest.MigrationID != spec.LegacyManifestID {
		return manifest, ErrConflict
	}
	return manifest, nil
}

// appendLegacy scans a durable keyset in bounded pages. Normal LIST entries and
// these keys share commitBatch's object-key uniqueness transaction.
func (s *Scanner) appendLegacy(ctx context.Context, attempt State, store transferstore.Store, emit func(transferstore.Object, bool) error) error {
	after := ""
	for {
		var page []LegacyKey
		err := s.store.View(func(tx *state.Tx) error {
			return tx.Scan(legacyKeys, parentKey(attempt.Spec.ParentID)+"/", after, 128, func(k string, raw json.RawMessage) error {
				var row LegacyKey
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				page = append(page, row)
				after = k
				return nil
			})
		})
		if err != nil {
			return err
		}
		for _, row := range page {
			if row.TimeFiltered {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var already bool
			err = s.store.View(func(tx *state.Tx) error {
				prior, found, err := getDiscoveredObject(tx, attempt.Spec.ParentID, row.Key)
				already = found && prior.SeenAttempt == attempt.Attempt
				return err
			})
			if err != nil {
				return err
			}
			if already {
				continue
			}
			obj, err := store.Stat(ctx, row.Key)
			missing := errors.Is(err, transferstore.ErrNotFound)
			if err != nil && !missing {
				return err
			}
			if missing {
				obj = transferstore.Object{Key: row.Key}
				if row.Size != nil {
					obj.Size = *row.Size
				}
			}
			if err := emit(obj, missing); err != nil {
				return err
			}
		}
		if len(page) < 128 {
			return nil
		}
	}
}

func LegacyHasKey(store *state.Store, parentID int64, key string) (found bool, err error) {
	err = store.View(func(tx *state.Tx) error {
		var row LegacyKey
		var e error
		found, e = tx.Get(legacyKeys, objectKey(parentID, key), &row)
		return e
	})
	return
}

// timeFiltered also guards fresh LIST output. Later completion/local-entry rows
// cannot erase a historical exclusion while rebuilding the same manifest.
func (s *Scanner) timeFiltered(parentID int64, key string) (excluded bool, err error) {
	err = s.store.View(func(tx *state.Tx) error {
		var row LegacyKey
		_, err := tx.Get(legacyKeys, objectKey(parentID, key), &row)
		excluded = row.TimeFiltered
		return err
	})
	return
}
