package task

import (
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func syncListingIndex(tx *state.Tx, p Parent) error {
	for _, index := range []struct {
		bucket  string
		include bool
	}{
		{"master-task-visible", !p.Deleted && !p.Purged},
		{"master-task-recycle", p.Deleted && !p.Purged},
	} {
		var err error
		if index.include {
			err = tx.Put(index.bucket, key(p.ID), p.ID)
		} else {
			err = tx.Delete(index.bucket, key(p.ID))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// EnsureListingIndex runs once when opening an older Go database. The schema
// marker and all entries commit together; concurrent creates use the same
// transaction lock and maintain these indexes through save.
func (r *Repository) EnsureListingIndex() error {
	return r.store.Update(func(tx *state.Tx) error {
		var version int
		if _, err := tx.Get("schema", "task-listing", &version); err != nil {
			return err
		}
		if version == 1 {
			return nil
		}
		after := ""
		for {
			count := 0
			if err := tx.Scan("master-tasks", "", after, 256, func(k string, raw json.RawMessage) error {
				var p Parent
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				after, count = k, count+1
				return syncListingIndex(tx, p)
			}); err != nil {
				return err
			}
			if count < 256 {
				return tx.Put("schema", "task-listing", 1)
			}
		}
	})
}

// Recent pages visible tasks or recycle-bin entries without scanning purged
// history. A single read transaction keeps the index and parent rows consistent.
func (r *Repository) Recent(before int64, limit int, deleted bool) (parents []Parent, err error) {
	if before < 0 || limit < 1 || limit > 10001 {
		return nil, errors.New("invalid task page")
	}
	bucket, cursor := "master-task-visible", ""
	if deleted {
		bucket = "master-task-recycle"
	}
	if before > 0 {
		cursor = key(before)
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.ScanReverse(bucket, cursor, limit, func(_ string, raw json.RawMessage) error {
			var id int64
			if err := json.Unmarshal(raw, &id); err != nil {
				return err
			}
			p, err := get(tx, id)
			if err == nil {
				parents = append(parents, p)
			}
			return err
		})
	})
	return
}
