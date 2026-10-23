package task

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// SchedulingState is a derived index. Keeping it in the parent means an older
// writer drops it rather than leaving an apparently valid, stale side index.
// A missing index is rebuilt from immutable assignments and their last reports.
type SchedulingState struct {
	Nodes map[string]PendingLoad `json:"nodes"`
}

type PendingLoad struct {
	Bytes    int64 `json:"bytes"`
	Objects  int64 `json:"objects"`
	Children int64 `json:"children"`
}

func childPending(child Child) PendingLoad {
	if child.Record != nil {
		switch child.Record.Status {
		case execution.Completed, execution.Failed, execution.PartialFailed, execution.Deleted:
			return PendingLoad{}
		}
	}
	load := PendingLoad{Objects: int64(len(child.Job.Items)), Children: 1}
	for _, item := range child.Job.Items {
		load.Bytes += item.Source.Size
	}
	if child.Record != nil {
		c := child.Record.Counters
		// Inventory imports can discover an object's size only on first read.
		load.Bytes = max(load.Bytes, c.TotalBytes)
		// Failed items remain reserved until the child ends. On resume these are
		// retried, whereas successful and skipped items remain durable outcomes.
		load.Objects = max(0, load.Objects-c.Succeeded-c.Skipped)
		load.Bytes = max(0, load.Bytes-c.SucceededBytes-c.SkippedBytes)
	}
	return load
}

func addLoad(to PendingLoad, change PendingLoad, sign int64) (PendingLoad, error) {
	for _, field := range []struct {
		target *int64
		value  int64
	}{{&to.Bytes, change.Bytes}, {&to.Objects, change.Objects}, {&to.Children, change.Children}} {
		if field.value < 0 || sign > 0 && *field.target > math.MaxInt64-field.value || sign < 0 && *field.target < field.value {
			return PendingLoad{}, errors.New("invalid pending task load")
		}
		*field.target += sign * field.value
	}
	return to, nil
}

func ensureScheduling(tx *state.Tx, p *Parent) error {
	if p.Scheduling != nil {
		return nil
	}
	p.Scheduling = &SchedulingState{Nodes: map[string]PendingLoad{}}
	after := ""
	for {
		count := 0
		err := tx.Scan("master-task-outstanding", key(p.ID)+"/", after, 256, func(k string, _ json.RawMessage) error {
			var child Child
			found, err := tx.Get("master-task-children", k, &child)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			load, err := addLoad(p.Scheduling.Nodes[child.Node.ID], childPending(child), 1)
			if err != nil {
				return err
			}
			p.Scheduling.Nodes[child.Node.ID] = load
			after, count = k, count+1
			return nil
		})
		if err != nil || count < 256 {
			return err
		}
	}
}

func changePending(p *Parent, old, current Child) error {
	node := current.Node.ID
	load, err := addLoad(p.Scheduling.Nodes[node], childPending(old), -1)
	if err != nil {
		return err
	}
	load, err = addLoad(load, childPending(current), 1)
	if err == nil {
		p.Scheduling.Nodes[node] = load
	}
	return err
}

// Other tasks share the same Agent execution slots. Read their compact loads
// once per dispatch page; completed history and object ledgers are not scanned.
func otherPending(tx *state.Tx, p Parent) (map[string]PendingLoad, error) {
	loads := map[string]PendingLoad{}
	after := ""
	for {
		count := 0
		err := tx.Scan("master-task-active", "", after, 128, func(k string, raw json.RawMessage) error {
			after, count = k, count+1
			var id int64
			if err := json.Unmarshal(raw, &id); err != nil {
				return err
			}
			if id == p.ID {
				return nil
			}
			other, err := get(tx, id)
			if err != nil {
				return err
			}
			rebuilt := other.Scheduling == nil
			if err := ensureScheduling(tx, &other); err != nil {
				return err
			}
			if rebuilt {
				// Persist only the derived field, in this assignment transaction.
				// Otherwise an unchanged older task is rescanned on every page.
				if err := tx.Put("master-tasks", key(id), other); err != nil {
					return err
				}
			}
			for node, pending := range other.Scheduling.Nodes {
				loads[node], err = addLoad(loads[node], pending, 1)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || count < 128 {
			return loads, err
		}
	}
}

func chooseNode(tx *state.Tx, p Parent, other map[string]PendingLoad, available map[string]bool) (Node, error) {
	node := p.Spec.Nodes[0]
	if p.Spec.DistributionMode != "BALANCED" {
		if available != nil && !available[node.ID] {
			return Node{}, errors.New("the selected execution node is unavailable")
		}
		return node, nil
	}
	bestCost, bestHistory := math.Inf(1), int64(math.MaxInt64)
	bestChildren := int64(math.MaxInt64)
	for _, candidate := range p.Spec.Nodes {
		if available != nil && !available[candidate.ID] {
			continue
		}
		pending, err := addLoad(p.Scheduling.Nodes[candidate.ID], other[candidate.ID], 1)
		if err != nil {
			return node, err
		}
		var history int64
		if _, err := tx.Get("master-task-load", key(p.ID)+"/"+candidate.ID, &history); err != nil {
			return node, err
		}
		// A fixed per-object cost accounts for request overhead on tiny objects.
		// Remaining bytes account for large objects. This is a placement weight,
		// not an ETA or a claim that every node has equal measured throughput.
		cost := float64(pending.Bytes) + float64(pending.Objects)*65536
		if cost < bestCost || cost == bestCost && (pending.Children < bestChildren || pending.Children == bestChildren && history < bestHistory) {
			node, bestCost, bestChildren, bestHistory = candidate, cost, pending.Children, history
		}
	}
	if math.IsInf(bestCost, 1) {
		return Node{}, errors.New("no selected execution node is available")
	}
	return node, nil
}
