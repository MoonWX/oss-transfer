package task

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

var ErrFailoverTarget = errors.New("no verified task node is available for failover")

// Retirement is permanent within a task. A returning node must not resume its
// old assignments or receive a renewed execution lease for this task.
type RetiredNode struct {
	RetiredAt time.Time `json:"retiredAt"`
}

type FailoverMigration struct {
	After     int       `json:"after"`
	Through   int       `json:"through"`
	StartedAt time.Time `json:"startedAt"`
}

type FailoverState struct {
	Enabled           bool                         `json:"enabled"`
	SharedDestination bool                         `json:"sharedDestination,omitempty"`
	Retired           map[string]RetiredNode       `json:"retired,omitempty"`
	Migrations        map[string]FailoverMigration `json:"migrations,omitempty"`
	ScannerNodeID     string                       `json:"scannerNodeId,omitempty"`
	ScannerEpoch      uint64                       `json:"scannerEpoch,omitempty"`
	ScannerSeeding    bool                         `json:"scannerSeeding,omitempty"`
	ScannerSeedAfter  int                          `json:"scannerSeedAfter,omitempty"`
	ScannerThrough    int                          `json:"scannerThrough,omitempty"`
}

func (p Parent) NodeRetired(id string) bool {
	if p.Failover == nil {
		return false
	}
	_, retired := p.Failover.Retired[id]
	return retired
}

func (p Parent) ActiveNodes() []Node {
	nodes := make([]Node, 0, len(p.Spec.Nodes))
	for _, node := range p.Spec.Nodes {
		if !p.NodeRetired(node.ID) {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func (p Parent) ScannerNode() Node {
	if len(p.Spec.Nodes) == 0 {
		return Node{}
	}
	if p.Failover != nil && p.Failover.ScannerNodeID != "" {
		for _, node := range p.Spec.Nodes {
			if node.ID == p.Failover.ScannerNodeID {
				return node
			}
		}
		return Node{}
	}
	return p.Spec.Nodes[0]
}

func (p Parent) ScannerEpoch() uint64 {
	if p.Failover == nil {
		return 0
	}
	return p.Failover.ScannerEpoch
}

func (p Parent) FailoverPending() bool {
	return p.Failover != nil && (len(p.Failover.Migrations) != 0 || p.Failover.ScannerSeeding)
}

func failoverParent(tx *state.Tx, id int64, generation uint64) (Parent, error) {
	p, err := get(tx, id)
	if err != nil {
		return p, err
	}
	if p.Generation != generation {
		return p, ErrGeneration
	}
	if p.Deleted || p.Purged || (p.Status != "RUNNING" && p.Status != "RESUMING") || p.ResumeAfter != nil || p.Failover == nil || !p.Failover.Enabled {
		return p, ErrState
	}
	return p, nil
}

// EnableFailover is called only after lease support and shared-directory
// identity have been verified. Persisting the schema fence prevents old writers
// from silently dropping the ownership state during a downgrade.
func (r *Repository) EnableFailover(id int64, generation uint64, sharedDestination bool) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Deleted || p.Purged || (p.Status != "RUNNING" && p.Status != "RESUMING") || len(p.Spec.Nodes) < 2 ||
			p.Spec.Source.Kind == "LOCAL" && !p.Spec.SharedLocalSource ||
			p.Spec.Destination.Kind == "LOCAL" && !sharedDestination ||
			sharedDestination && p.Spec.Destination.Kind != "LOCAL" {
			return ErrState
		}
		if p.Failover != nil && p.Failover.Enabled {
			if p.Failover.SharedDestination != sharedDestination {
				return ErrConflict
			}
			return errUnchanged
		}
		if err := state.RequireSchema(tx, "master", state.SchemaTaskLeases); err != nil {
			return err
		}
		p.Failover = &FailoverState{Enabled: true, SharedDestination: sharedDestination}
		p.Spec.SharedLocalDestination = sharedDestination
		return r.save(tx, p)
	}))
	return
}

// BeginFailover records intent before any reassignment or remote call. The
// caller must first establish that the node's last execution lease has expired.
func (r *Repository) BeginFailover(id int64, generation uint64, nodeID string) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		if p.NodeRetired(nodeID) {
			return errUnchanged
		}
		selected := false
		for _, node := range p.Spec.Nodes {
			selected = selected || node.ID == nodeID
		}
		if !selected {
			return ErrFailoverTarget
		}
		if len(p.ActiveNodes()) < 2 {
			return ErrFailoverTarget
		}
		if p.BudgetRevision == math.MaxUint64 {
			return ErrConflict
		}
		now := r.now().UTC()
		if p.Failover.Retired == nil {
			p.Failover.Retired = map[string]RetiredNode{}
		}
		if p.Failover.Migrations == nil {
			p.Failover.Migrations = map[string]FailoverMigration{}
		}
		p.Failover.Retired[nodeID] = RetiredNode{RetiredAt: now}
		p.Failover.Migrations[nodeID] = FailoverMigration{Through: p.ChildCount, StartedAt: now}
		p.BudgetRevision++
		return r.save(tx, p)
	}))
	return
}

// PrepareFailover changes at most 128 child records in one transaction. Its
// cursor is committed with the new ownership, result ledger and all indexes.
// available contains only original task members whose source/target access and
// current lease have been acknowledged. No network I/O occurs under this lock.
func (r *Repository) PrepareFailover(id int64, generation uint64, nodeID string, available map[string]bool) (moved []Child, ready bool, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		migration, exists := p.Failover.Migrations[nodeID]
		if !exists {
			if !p.NodeRetired(nodeID) {
				return ErrState
			}
			ready = true
			return errUnchanged
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		if err := ensureChildOrder(tx, &p); err != nil {
			return err
		}
		var page []Child
		after := ""
		if migration.After > 0 {
			after = childKey(id, migration.After)
		}
		if err := tx.Scan("master-task-children", key(id)+"/", after, 128, func(_ string, raw json.RawMessage) error {
			var child Child
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			page = append(page, child)
			return nil
		}); err != nil {
			return err
		}
		// Reassignment is balanced even when the original placement was affinity.
		placement := p
		placement.Spec.DistributionMode = "BALANCED"
		other, err := otherPending(tx, p)
		if err != nil {
			return err
		}
		if available == nil {
			available = map[string]bool{}
		}
		for _, child := range page {
			if child.Job.Number > migration.Through {
				break
			}
			migration.After = child.Job.Number
			if child.Node.ID != nodeID || child.Record != nil && child.Record.Status == execution.Completed {
				continue
			}
			node, err := chooseNode(tx, placement, other, available)
			if err != nil {
				return ErrFailoverTarget
			}
			previous := child
			if child.Job.Assignment == math.MaxUint64 {
				return ErrConflict
			}
			child.Node, child.Accepted = node, false
			child.Job.Assignment++
			child.Job.Generation = generation
			child.Job.SharedLocalDestination = p.Failover.SharedDestination
			if child.Job.Source.Kind == "LOCAL" {
				child.Job.Source.Root = node.LocalRoot
			}
			if child.Job.Destination.Kind == "LOCAL" {
				child.Job.Destination.Root = node.LocalRoot
			}
			if child.Record != nil {
				seed, err := failoverSeed(child.Job, *child.Record, r.now().UTC())
				if err != nil {
					return err
				}
				countChild(&p, child.Record.Status, -1)
				addCounters(&p.Counters, child.Record.Counters, -1)
				addCounters(&p.Counters, seed.Counters, 1)
				child.Record = &seed
				if err := syncExceptionIndex(tx, p, child, seed); err != nil {
					return err
				}
			}
			if err := changePending(&p, previous, child); err != nil {
				return err
			}
			if err := syncChildOrder(tx, &p, &previous, child); err != nil {
				return err
			}
			if err := tx.Put("master-task-children", childKey(id, child.Job.Number), child); err != nil {
				return err
			}
			if err := tx.Put("master-task-outstanding", childKey(id, child.Job.Number), child.Job.Number); err != nil {
				return err
			}
			pending := childPending(child)
			if pending.Objects > (math.MaxInt64-pending.Bytes)/65536 {
				return ErrConflict
			}
			cost := pending.Bytes + pending.Objects*65536
			var history int64
			loadKey := key(id) + "/" + node.ID
			if _, err := tx.Get("master-task-load", loadKey, &history); err != nil {
				return err
			}
			if history > math.MaxInt64-cost {
				return ErrConflict
			}
			if err := tx.Put("master-task-load", loadKey, history+cost); err != nil {
				return err
			}
			moved = append(moved, child)
		}
		if migration.After >= migration.Through || len(page) < 128 {
			delete(p.Failover.Migrations, nodeID)
			ready = true
		} else {
			p.Failover.Migrations[nodeID] = migration
		}
		return r.save(tx, p)
	}))
	if err != nil {
		return nil, false, err
	}
	return
}

func failoverSeed(job execution.Job, previous execution.Record, now time.Time) (execution.Record, error) {
	seed := previous
	seed.Job, seed.Status, seed.Error, seed.FinishedAt = job, execution.Recoverable, "", nil
	seed.UpdatedAt = now
	timing := previous.ExecutionTime()
	timing.Transition(false, previous.UpdatedAt)
	seed.ActiveTime = &timing
	byID := make(map[string]execution.ItemResult, len(previous.Results))
	for _, result := range previous.Results {
		if _, duplicate := byID[result.ID]; duplicate {
			return execution.Record{}, ErrConflict
		}
		byID[result.ID] = result
	}
	seed.Results = make([]execution.ItemResult, len(job.Items))
	seed.Counters = execution.Counters{Total: int64(len(job.Items))}
	for i, item := range job.Items {
		result, found := byID[item.ID]
		if !found && len(previous.Results) != 0 {
			return execution.Record{}, ErrConflict
		}
		delete(byID, item.ID)
		switch result.Status {
		case execution.ItemSucceeded, execution.ItemSkipped:
		case "", execution.ItemPending, execution.ItemFailed:
			result = execution.ItemResult{ID: item.ID, Status: execution.ItemPending, ResolvedSource: result.ResolvedSource}
		default:
			return execution.Record{}, ErrConflict
		}
		seed.Results[i] = result
		size := execution.EffectiveSource(item, result).Size
		if size < 0 || seed.Counters.TotalBytes > math.MaxInt64-size {
			return execution.Record{}, ErrConflict
		}
		seed.Counters.TotalBytes += size
		switch result.Status {
		case execution.ItemSucceeded:
			seed.Counters.Succeeded++
			seed.Counters.SucceededBytes += size
		case execution.ItemSkipped:
			seed.Counters.Skipped++
			seed.Counters.SkippedBytes += size
		}
	}
	if len(byID) != 0 || seed.Counters.Succeeded != previous.Counters.Succeeded || seed.Counters.Skipped != previous.Counters.Skipped ||
		seed.Counters.SucceededBytes != previous.Counters.SucceededBytes || seed.Counters.SkippedBytes != previous.Counters.SkippedBytes {
		return execution.Record{}, ErrConflict
	}
	return seed, nil
}

// BeginScannerFailover discards only scan progress whose batches were never
// assigned by the Master. Existing child assignments form the replacement
// scanner's immutable seed; scanning resumes after that seed has been imported.
func (r *Repository) BeginScannerFailover(id int64, generation uint64, targetNodeID string) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		if p.ScannerNode().ID == targetNodeID && p.Failover.ScannerSeeding {
			return errUnchanged
		}
		if !p.NodeRetired(p.ScannerNode().ID) || p.NodeRetired(targetNodeID) {
			return ErrState
		}
		selected := false
		for _, node := range p.ActiveNodes() {
			selected = selected || node.ID == targetNodeID
		}
		if !selected {
			return ErrFailoverTarget
		}
		if p.Failover.ScannerEpoch == math.MaxUint64 {
			return ErrConflict
		}
		p.Failover.ScannerNodeID = targetNodeID
		p.Failover.ScannerEpoch++
		p.Failover.ScannerSeeding = true
		p.Failover.ScannerSeedAfter = 0
		p.Failover.ScannerThrough = p.DispatchedThrough
		p.ScannedObjects, p.ScannedBytes, p.ScannedBatches = p.DispatchedObjects, p.DispatchedBytes, p.DispatchedThrough
		p.ScanComplete, p.Error = false, ""
		return r.save(tx, p)
	}))
	return
}

func (r *Repository) AdvanceScannerSeed(id int64, generation, epoch uint64, after int) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		f := p.Failover
		if f.ScannerEpoch != epoch {
			return ErrGeneration
		}
		if !f.ScannerSeeding || p.NodeRetired(f.ScannerNodeID) || after < 0 || after > f.ScannerThrough {
			return ErrState
		}
		if after <= f.ScannerSeedAfter {
			return errUnchanged
		}
		f.ScannerSeedAfter = after
		return r.save(tx, p)
	}))
	return
}

func (r *Repository) FinishScannerSeed(id int64, generation, epoch uint64) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		f := p.Failover
		if f.ScannerEpoch != epoch {
			return ErrGeneration
		}
		if p.NodeRetired(f.ScannerNodeID) || f.ScannerSeedAfter != f.ScannerThrough {
			return ErrState
		}
		if !f.ScannerSeeding {
			return errUnchanged
		}
		f.ScannerSeeding = false
		return r.save(tx, p)
	}))
	return
}
