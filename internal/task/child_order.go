package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const childOrderBucket = "master-task-child-order"

// ChildOrderState is derived from the child records. An older writer drops this
// field when it saves the parent, so a subsequent upgrade rebuilds stale indexes.
type ChildOrderState struct {
	Version int                 `json:"version"`
	All     *AllChildOrderState `json:"all,omitempty"`
}

func childStatus(c Child) execution.Status {
	if c.Record == nil {
		return execution.Waiting
	}
	return c.Record.Status
}

// Keep this order aligned with activeSubtaskPriority in the console. Unknown
// unfinished states follow the known states, so they remain discoverable.
func childPriority(c Child) int {
	switch childStatus(c) {
	case execution.Running, "SCAN_COMPLETED":
		return 0
	case "RECOVERING", "RESUMING":
		return 1
	case "STOPPING":
		return 2
	case execution.Recoverable, "WAITING_RECOVERY":
		return 3
	case execution.Waiting, "PENDING", "CREATED":
		return 4
	case execution.Completed, execution.Failed, execution.PartialFailed, execution.Deleted:
		return -1
	default:
		return 5
	}
}

func ChildOrderCursor(c Child) string {
	return fmt.Sprintf("%d/%010d", childPriority(c), c.Job.Number)
}

func ValidChildOrderCursor(cursor string) bool {
	parts := strings.Split(cursor, "/")
	if len(parts) != 2 || len(parts[0]) != 1 || parts[0][0] < '0' || parts[0][0] > '5' {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	return err == nil && n > 0 && fmt.Sprintf("%010d", n) == parts[1]
}

func ensureChildOrder(tx *state.Tx, p *Parent) error {
	if p.ChildOrder != nil && p.ChildOrder.Version == 1 {
		return nil
	}
	prefix := key(p.ID) + "/"
	// A downgrade may leave obsolete ordering keys behind.
	for {
		var keys []string
		if err := tx.Scan(childOrderBucket, prefix, "", 256, func(k string, _ json.RawMessage) error {
			keys = append(keys, k)
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if err := tx.Delete(childOrderBucket, k); err != nil {
				return err
			}
		}
		if len(keys) < 256 {
			break
		}
	}
	after := ""
	for {
		count := 0
		err := tx.Scan("master-task-outstanding", prefix, after, 256, func(k string, _ json.RawMessage) error {
			var child Child
			found, err := tx.Get("master-task-children", k, &child)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			if childPriority(child) >= 0 {
				if err := tx.Put(childOrderBucket, prefix+ChildOrderCursor(child), child.Job.Number); err != nil {
					return err
				}
			}
			after, count = k, count+1
			return nil
		})
		if err != nil {
			return err
		}
		if count < 256 {
			break
		}
	}
	if p.ChildOrder == nil {
		p.ChildOrder = &ChildOrderState{}
	}
	p.ChildOrder.Version = 1
	return nil
}

func syncChildOrder(tx *state.Tx, p *Parent, previous *Child, current Child) error {
	if previous != nil && childStatus(*previous) == childStatus(current) {
		return nil
	}
	prefix := key(p.ID) + "/"
	if previous != nil && childPriority(*previous) >= 0 {
		if err := tx.Delete(childOrderBucket, prefix+ChildOrderCursor(*previous)); err != nil {
			return err
		}
	}
	if childPriority(current) >= 0 {
		if err := tx.Put(childOrderBucket, prefix+ChildOrderCursor(current), current.Job.Number); err != nil {
			return err
		}
	}
	return syncAllChildOrder(tx, p, previous, current)
}

// ActiveChildren reads only one indexed window, regardless of how many older
// waiting or completed children exist. Pagination never affects dispatch order.
func (r *Repository) ActiveChildren(id int64, after string, limit int, known []int) (rows, updates []Child, err error) {
	if limit < 1 || limit > 256 || len(known) > 200 || after != "" && !ValidChildOrderCursor(after) {
		return nil, nil, errors.New("invalid active child cursor or page size")
	}
	p, err := r.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if p.ChildOrder == nil || p.ChildOrder.Version != 1 {
		err = r.store.Update(func(tx *state.Tx) error {
			current, err := get(tx, id)
			if err != nil || current.ChildOrder != nil && current.ChildOrder.Version == 1 {
				return err
			}
			if err := ensureChildOrder(tx, &current); err != nil {
				return err
			}
			return save(tx, current)
		})
		if err != nil {
			return nil, nil, err
		}
	}
	err = r.store.View(func(tx *state.Tx) error {
		// A visible row can move ahead of its old cursor when its state changes.
		// Read those bounded identities in the same snapshot as the new window.
		for _, number := range known {
			var child Child
			found, err := tx.Get("master-task-children", childKey(id, number), &child)
			if err != nil {
				return err
			}
			if found {
				updates = append(updates, child)
			}
		}
		prefix := key(id) + "/"
		start := ""
		if after != "" {
			start = prefix + after
		}
		return tx.Scan(childOrderBucket, prefix, start, limit, func(_ string, raw json.RawMessage) error {
			var number int
			if err := json.Unmarshal(raw, &number); err != nil {
				return err
			}
			var child Child
			found, err := tx.Get("master-task-children", childKey(id, number), &child)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			rows = append(rows, child)
			return nil
		})
	})
	return
}
