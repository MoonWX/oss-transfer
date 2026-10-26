package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const allChildOrderPage = 256
const allChildOrderBucket = "master-task-child-priority"

// Each page and its cursor commit together. A fresh revision makes an older
// binary's stale index invisible before bounded rebuilding and cleanup begin.
type AllChildOrderState struct {
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
	After    string `json:"after,omitempty"`
	Ready    bool   `json:"ready"`
	Cleaned  bool   `json:"cleaned,omitempty"`
}

func allChildPriority(c Child) int {
	switch childStatus(c) {
	case execution.Failed:
		return 0
	case execution.PartialFailed:
		return 1
	case execution.Recoverable, "WAITING_RECOVERY":
		return 2
	case execution.Running, "SCAN_COMPLETED":
		return 3
	case "RECOVERING", "RESUMING":
		return 4
	case "STOPPING":
		return 5
	case execution.Waiting, "PENDING", "CREATED":
		return 6
	case "STOPPED":
		return 7
	case execution.Completed:
		return 9
	case execution.Deleted:
		return 10
	default:
		return 8
	}
}

func AllChildOrderCursor(c Child) string {
	return fmt.Sprintf("%02d/%010d", allChildPriority(c), c.Job.Number)
}

func ValidAllChildOrderCursor(cursor string) bool {
	parts := strings.Split(cursor, "/")
	if len(parts) != 2 {
		return false
	}
	rank, err := strconv.Atoi(parts[0])
	if err != nil || rank < 0 || rank > 10 || fmt.Sprintf("%02d", rank) != parts[0] {
		return false
	}
	number, err := strconv.Atoi(parts[1])
	return err == nil && number > 0 && fmt.Sprintf("%010d", number) == parts[1]
}

func allChildPrefix(p Parent) string {
	return fmt.Sprintf("%s/all/%020d/", key(p.ID), p.ChildOrder.All.Revision)
}

func syncAllChildOrder(tx *state.Tx, p *Parent, previous *Child, current Child) error {
	if p.ChildOrder == nil || p.ChildOrder.All == nil || p.ChildOrder.All.Version != 1 {
		return nil
	}
	prefix := allChildPrefix(*p)
	if previous != nil {
		if err := tx.Delete(allChildOrderBucket, prefix+AllChildOrderCursor(*previous)); err != nil {
			return err
		}
	}
	return tx.Put(allChildOrderBucket, prefix+AllChildOrderCursor(current), current.Job.Number)
}

func prepareAllChildOrderPage(tx *state.Tx, p *Parent) (ready bool, err error) {
	if p.ChildOrder == nil {
		p.ChildOrder = &ChildOrderState{}
	}
	if p.ChildOrder.All == nil || p.ChildOrder.All.Version != 1 {
		revision, err := tx.NextID(allChildOrderBucket)
		if err != nil {
			return false, err
		}
		p.ChildOrder.All = &AllChildOrderState{Version: 1, Revision: revision}
	}
	index := p.ChildOrder.All
	if !index.Ready {
		count := 0
		err := tx.Scan("master-task-children", key(p.ID)+"/", index.After, allChildOrderPage, func(k string, raw json.RawMessage) error {
			// Do not materialize the object's immutable assignment twice just to
			// index one status and one number from a large child record.
			var summary struct {
				Job struct {
					Number int `json:"number"`
				} `json:"job"`
				Record *struct {
					Status execution.Status `json:"status"`
				} `json:"record"`
			}
			if err := json.Unmarshal(raw, &summary); err != nil {
				return err
			}
			child := Child{Job: execution.Job{Number: summary.Job.Number}}
			if summary.Record != nil {
				child.Record = &execution.Record{Status: summary.Record.Status}
			}
			if child.Job.Number < 1 {
				return errors.New("invalid child ordering identity")
			}
			if err := syncAllChildOrder(tx, p, nil, child); err != nil {
				return err
			}
			index.After, count = k, count+1
			return nil
		})
		if err != nil {
			return false, err
		}
		index.Ready = count < allChildOrderPage
	} else if !index.Cleaned {
		// Old revisions sort before the current one. Remove at most one page;
		// the live prefix terminates cleanup without scanning its children.
		var stale []string
		current := allChildPrefix(*p)
		stop := errors.New("live index reached")
		err := tx.Scan(allChildOrderBucket, key(p.ID)+"/all/", "", allChildOrderPage, func(k string, _ json.RawMessage) error {
			if k >= current {
				index.Cleaned = true
				return stop
			}
			stale = append(stale, k)
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			return false, err
		}
		for _, k := range stale {
			if err := tx.Delete(allChildOrderBucket, k); err != nil {
				return false, err
			}
		}
		index.Cleaned = index.Cleaned || len(stale) < allChildOrderPage
	}
	return index.Ready, save(tx, *p)
}

// PriorityChildren provides global ordering for all child states. Upgrades
// advance in small transactions, with a bounded request budget and a durable
// cursor so interruption never restarts the work or monopolizes the writer.
func (r *Repository) PriorityChildren(ctx context.Context, id int64, after string, limit int, known []int) (rows, updates []Child, ready bool, err error) {
	if limit < 1 || limit > 256 || len(known) > 200 || after != "" && !ValidAllChildOrderCursor(after) {
		return nil, nil, false, errors.New("invalid child priority cursor or page size")
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for page := 0; page < 8; page++ {
		if err = ctx.Err(); err != nil {
			return
		}
		var p Parent
		p, err = r.Get(id)
		if err != nil {
			return
		}
		if p.ChildOrder != nil && p.ChildOrder.All != nil && p.ChildOrder.All.Version == 1 && p.ChildOrder.All.Ready && p.ChildOrder.All.Cleaned {
			ready = true
			break
		}
		err = r.store.Update(func(tx *state.Tx) error {
			current, err := get(tx, id)
			if err != nil {
				return err
			}
			if current.Purged || current.Deleted {
				return ErrState
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			ready, err = prepareAllChildOrderPage(tx, &current)
			return err
		})
		if err != nil || ready || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || !ready {
		return
	}
	err = r.store.View(func(tx *state.Tx) error {
		parent, err := get(tx, id)
		if err != nil {
			return err
		}
		if parent.ChildOrder == nil || parent.ChildOrder.All == nil || !parent.ChildOrder.All.Ready {
			ready = false
			return nil
		}
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
		prefix := allChildPrefix(parent)
		start := ""
		if after != "" {
			start = prefix + after
		}
		return tx.Scan(allChildOrderBucket, prefix, start, limit, func(_ string, raw json.RawMessage) error {
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
