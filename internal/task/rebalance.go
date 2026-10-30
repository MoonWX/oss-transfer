package task

import (
	"encoding/json"
	"math"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// Two bounded passes over outstanding work, never completed task history.
// Pending is written before the donor is asked to relinquish an assignment.
type RebalanceState struct {
	Targets map[string]bool `json:"targets"`
	After   int             `json:"after,omitempty"`
	Pass    int             `json:"pass,omitempty"`
	Pending *QueuedHandoff  `json:"pending,omitempty"`
}
type QueuedHandoff struct {
	Number     int    `json:"number"`
	Generation uint64 `json:"generation"`
	Assignment uint64 `json:"assignment"`
	OwnerEpoch uint64 `json:"ownerEpoch"`
	Source     Node   `json:"source"`
	Target     Node   `json:"target"`
}

func (p Parent) HandoffPending(number int) bool {
	return p.Failover != nil && p.Failover.Rebalance != nil && p.Failover.Rebalance.Pending != nil && p.Failover.Rebalance.Pending.Number == number
}
func pendingWeight(v PendingLoad) float64 { return float64(v.Bytes) + float64(v.Objects)*65536 }

func (p Parent) NeedsQueuedRebalance() bool {
	if p.Failover == nil || !p.Failover.Enabled {
		return false
	}
	if p.Failover.Rebalance != nil {
		return true
	}
	for _, node := range p.ActiveNodes() {
		if p.NodeEpoch(node.ID) > p.Failover.RebalancedEpochs[node.ID] {
			return true
		}
	}
	return false
}

// Nodes which rejoined before queue handoff was supported also receive one
// pass after upgrade. Recording the epoch bounds this to a membership event.
func (r *Repository) EnsureQueuedRebalance(id int64, generation uint64) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		changed := false
		for _, node := range p.ActiveNodes() {
			epoch := p.NodeEpoch(node.ID)
			if epoch == 0 || p.Failover.RebalancedEpochs[node.ID] >= epoch {
				continue
			}
			if p.Failover.Rebalance == nil {
				p.Failover.Rebalance = &RebalanceState{}
			}
			if p.Failover.Rebalance.Targets == nil {
				p.Failover.Rebalance.Targets = map[string]bool{}
			}
			p.Failover.Rebalance.Targets[node.ID] = true
			p.Failover.Rebalance.After, p.Failover.Rebalance.Pass = 0, 0
			if p.Failover.RebalancedEpochs == nil {
				p.Failover.RebalancedEpochs = map[string]uint64{}
			}
			p.Failover.RebalancedEpochs[node.ID] = epoch
			changed = true
		}
		if !changed {
			return errUnchanged
		}
		return r.save(tx, p)
	}))
	return
}

// NextQueuedHandoff persists at most one transfer intent. Only idle work whose
// move improves the existing byte/object load score is eligible. Affinity keeps
// its selected primary; balanced mode considers all verified available members.
func (r *Repository) NextQueuedHandoff(id int64, generation uint64, available map[string]bool) (handoff *QueuedHandoff, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		b := p.Failover.Rebalance
		if b == nil {
			return errUnchanged
		}
		if b.Pending != nil {
			var c Child
			found, err := tx.Get("master-task-children", childKey(id, b.Pending.Number), &c)
			if err != nil {
				return err
			}
			if !found || c.Node.ID != b.Pending.Source.ID || c.Job.Assignment != b.Pending.Assignment || c.Job.OwnerEpoch != b.Pending.OwnerEpoch {
				b.Pending = nil
				return r.save(tx, p)
			}
			handoff = b.Pending
			return errUnchanged
		}
		if p.Status != "RUNNING" || p.FailoverPending() {
			return errUnchanged
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		other, err := otherPending(tx, p)
		if err != nil {
			return err
		}
		var page []Child
		if err := tx.Scan("master-task-outstanding", key(id)+"/", childKey(id, b.After), 64, func(k string, _ json.RawMessage) error {
			var c Child
			found, err := tx.Get("master-task-children", k, &c)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			page = append(page, c)
			return nil
		}); err != nil {
			return err
		}
		for _, c := range page {
			b.After = c.Job.Number
			if !available[c.Node.ID] || p.NodeRetired(c.Node.ID) || c.Record != nil && !c.Record.QueuedForHandoff() {
				continue
			}
			target, err := chooseNode(tx, p, other, available)
			if err != nil {
				continue
			}
			if target.ID == c.Node.ID || !b.Targets[target.ID] {
				continue
			}
			from, err := addLoad(p.Scheduling.Nodes[c.Node.ID], other[c.Node.ID], 1)
			if err != nil {
				return err
			}
			to, err := addLoad(p.Scheduling.Nodes[target.ID], other[target.ID], 1)
			if err != nil {
				return err
			}
			weight := pendingWeight(childPending(c))
			// Avoid moving a large child if that merely swaps the imbalance.
			if p.Spec.DistributionMode == "BALANCED" && (weight <= 0 || weight >= pendingWeight(from)-pendingWeight(to)) {
				continue
			}
			if err := state.RequireSchema(tx, "master", state.SchemaTaskHandoff); err != nil {
				return err
			}
			b.Pending = &QueuedHandoff{Number: c.Job.Number, Generation: c.Job.Generation, Assignment: c.Job.Assignment, OwnerEpoch: c.Job.OwnerEpoch, Source: c.Node, Target: target}
			handoff = b.Pending
			return r.save(tx, p)
		}
		if len(page) < 64 {
			b.After = 0
			b.Pass++
			if b.Pass >= 2 {
				p.Failover.Rebalance = nil
			}
		}
		return r.save(tx, p)
	}))
	return
}

// FinishQueuedHandoff commits new ownership only after the donor durably
// acknowledged revocation. Replays after a generation change can finish the
// old intent; a concurrent failover's newer assignment always wins.
func (r *Repository) FinishQueuedHandoff(id int64, intent QueuedHandoff, released bool, available map[string]bool) (moved *Child, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Deleted || p.Purged || p.Failover == nil || p.Failover.Rebalance == nil || p.Failover.Rebalance.Pending == nil {
			return errUnchanged
		}
		b := p.Failover.Rebalance
		if *b.Pending != intent {
			return ErrConflict
		}
		var c Child
		found, err := tx.Get("master-task-children", childKey(id, intent.Number), &c)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if !released || c.Node.ID != intent.Source.ID || c.Job.Assignment != intent.Assignment || c.Job.OwnerEpoch != intent.OwnerEpoch {
			b.Pending = nil
			return r.save(tx, p)
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		if err := ensureChildOrder(tx, &p); err != nil {
			return err
		}
		target := intent.Target
		if p.NodeRetired(target.ID) || !available[target.ID] {
			other, err := otherPending(tx, p)
			if err != nil {
				return err
			}
			target, err = chooseNode(tx, p, other, available)
			if err != nil {
				return ErrFailoverTarget
			}
		}
		if c.Job.Assignment == math.MaxUint64 {
			return ErrConflict
		}
		previous := c
		c.Node, c.Accepted = target, false
		c.Job.Generation, c.Job.Assignment, c.Job.OwnerEpoch = p.Generation, c.Job.Assignment+1, p.NodeEpoch(target.ID)
		if c.Job.Source.Kind == "LOCAL" {
			c.Job.Source.Root = target.LocalRoot
		}
		if c.Job.Destination.Kind == "LOCAL" {
			c.Job.Destination.Root = target.LocalRoot
		}
		if c.Record != nil {
			record := *c.Record
			record.Job = c.Job
			c.Record = &record
		}
		if err := changePending(&p, previous, c); err != nil {
			return err
		}
		if err := syncChildOrder(tx, &p, &previous, c); err != nil {
			return err
		}
		if err := tx.Put("master-task-children", childKey(id, c.Job.Number), c); err != nil {
			return err
		}
		b.Pending = nil
		if err := r.save(tx, p); err != nil {
			return err
		}
		moved = &c
		return nil
	}))
	return
}
