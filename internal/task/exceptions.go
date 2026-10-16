package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type Exception struct {
	ParentID        int64              `json:"parentTaskId"`
	ChildID         string             `json:"childTaskId"`
	Generation      uint64             `json:"generation"`
	Attempt         uint64             `json:"attempt"`
	Direction       string             `json:"direction"`
	Node            string             `json:"node"`
	Source          execution.Endpoint `json:"source"`
	Destination     execution.Endpoint `json:"destination"`
	Key             string             `json:"key"`
	DestinationKey  string             `json:"destinationKey"`
	Size            int64              `json:"size"`
	Error           string             `json:"error"`
	TargetCommitted bool               `json:"targetCommitted"`
	ObservedAt      time.Time          `json:"observedAt"`
	Identity        string             `json:"identity"`
}
type ExceptionFilter struct {
	ParentID                                                   int64
	Node, Direction, SourceBucket, DestinationBucket, Endpoint string
	LocalOnly                                                  bool
}

func syncExceptionIndex(tx *state.Tx, p Parent, child Child, record execution.Record) error {
	prefix := childKey(p.ID, child.Job.Number) + "/"
	// At most 128 items per child, so replacing this index in the same ledger
	// transaction is bounded and cannot expose a half-updated set of failures.
	var previous []string
	if err := tx.Scan("master-task-exceptions", prefix, "", 129, func(k string, _ json.RawMessage) error { previous = append(previous, k); return nil }); err != nil {
		return err
	}
	for _, k := range previous {
		if err := tx.Delete("master-task-exceptions", k); err != nil {
			return err
		}
	}
	if p.Purged {
		return nil
	}
	items := make(map[string]execution.Item, len(record.Job.Items))
	for _, item := range record.Job.Items {
		items[item.ID] = item
	}
	for _, result := range record.Results {
		if result.Status != execution.ItemFailed {
			continue
		}
		item, ok := items[result.ID]
		if !ok {
			return errors.New("failed ledger references an unknown object")
		}
		item.Source = execution.EffectiveSource(item, result)
		entry := Exception{ParentID: p.ID, ChildID: child.Job.ChildID, Generation: record.Job.Generation, Attempt: record.Attempt, Direction: p.Spec.Direction, Node: child.Node.Address, Source: child.Job.Source, Destination: child.Job.Destination, Key: item.Source.Key, DestinationKey: item.DestinationKey, Size: item.Source.Size, Error: result.Error, TargetCommitted: result.TargetCommitted, ObservedAt: record.UpdatedAt}
		entry.Identity = digest(struct {
			Generation, Attempt uint64
			ID, Error           string
		}{record.Job.Generation, record.Attempt, result.ID, result.Error})
		if err := tx.Put("master-task-exceptions", prefix+digest(result.ID), entry); err != nil {
			return err
		}
	}
	return nil
}

// EnsureExceptionIndex upgrades an existing Go database once. All subsequent
// updates happen atomically with Observe, so HTTP queries never rescan jobs.
func (r *Repository) EnsureExceptionIndex() error {
	return r.store.Update(func(tx *state.Tx) error {
		var version int
		if _, err := tx.Get("schema", "task-exceptions", &version); err != nil {
			return err
		}
		if version == 1 {
			return nil
		}
		after := ""
		for {
			var children []Child
			last := ""
			if err := tx.Scan("master-task-children", "", after, 128, func(k string, raw json.RawMessage) error {
				var c Child
				if err := json.Unmarshal(raw, &c); err != nil {
					return err
				}
				children = append(children, c)
				last = k
				return nil
			}); err != nil {
				return err
			}
			for _, c := range children {
				if c.Record == nil {
					continue
				}
				p, err := get(tx, c.Job.ParentID)
				if err != nil {
					return err
				}
				if err := syncExceptionIndex(tx, p, c, *c.Record); err != nil {
					return err
				}
			}
			if len(children) < 128 {
				break
			}
			after = last
		}
		return tx.Put("schema", "task-exceptions", 1)
	})
}

func exceptionMatches(e Exception, f ExceptionFilter) bool {
	if f.ParentID > 0 && e.ParentID != f.ParentID || f.Node != "" && e.Node != f.Node || f.Direction != "" && e.Direction != f.Direction || f.LocalOnly && e.Direction == "OSS_TO_OSS" || f.SourceBucket != "" && e.Source.Bucket != f.SourceBucket || f.DestinationBucket != "" && e.Destination.Bucket != f.DestinationBucket {
		return false
	}
	if f.Endpoint != "" {
		endpoint := func(e execution.Endpoint) string {
			if e.Kind == "LOCAL" {
				return e.Root
			}
			return e.Ref + "/" + e.Bucket
		}
		if !strings.Contains(endpoint(e.Source), f.Endpoint) && !strings.Contains(endpoint(e.Destination), f.Endpoint) {
			return false
		}
	}
	return true
}
func visitExceptions(ctx context.Context, tx *state.Tx, filter ExceptionFilter, visit func(string, Exception) error) error {
	prefix := ""
	if filter.ParentID > 0 {
		prefix = key(filter.ParentID) + "/"
	}
	after := ""
	var parent Parent
	for {
		count := 0
		err := tx.Scan("master-task-exceptions", prefix, after, 256, func(k string, raw json.RawMessage) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			count++
			after = k
			var entry Exception
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if !exceptionMatches(entry, filter) {
				return nil
			}
			if parent.ID != entry.ParentID {
				var err error
				parent, err = get(tx, entry.ParentID)
				if err != nil {
					return err
				}
			}
			if parent.Deleted || parent.Purged {
				return nil
			}
			var hidden string
			if _, err := tx.Get("master-task-exceptions-hidden", k, &hidden); err != nil {
				return err
			}
			if hidden == entry.Identity {
				return nil
			}
			return visit(k, entry)
		})
		if err != nil {
			return err
		}
		if count < 256 {
			return nil
		}
	}
}
func (r *Repository) VisitExceptions(ctx context.Context, filter ExceptionFilter, visit func(Exception) error) error {
	return r.store.View(func(tx *state.Tx) error {
		return visitExceptions(ctx, tx, filter, func(_ string, e Exception) error { return visit(e) })
	})
}
func (r *Repository) ClearExceptions(ctx context.Context, filter ExceptionFilter) (int64, error) {
	var count int64
	err := r.store.Update(func(tx *state.Tx) error {
		return visitExceptions(ctx, tx, filter, func(k string, e Exception) error {
			if err := tx.Put("master-task-exceptions-hidden", k, e.Identity); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
