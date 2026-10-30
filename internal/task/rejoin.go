package task

import (
	"math"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (p Parent) CanRejoinScanner(nodeID string) bool {
	return p.ScannerNode().ID != nodeID || p.ScanComplete && p.DispatchedThrough >= p.ScannedBatches
}

func (p Parent) NodeEpoch(nodeID string) uint64 {
	if p.Failover == nil {
		return 0
	}
	return p.Failover.NodeEpochs[nodeID]
}

func (r *Repository) BeginNodeRejoin(id int64, generation uint64, nodeID string) (p Parent, epoch uint64, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		if !p.NodeRetired(nodeID) || len(p.Failover.Migrations) > 0 || p.Failover.ScannerSeeding || !p.CanRejoinScanner(nodeID) {
			return ErrState
		}
		if value := p.Failover.Rejoining[nodeID]; value > 0 {
			epoch = value
			return errUnchanged
		}
		if p.NodeEpoch(nodeID) == math.MaxUint64 {
			return ErrConflict
		}
		if err := state.RequireSchema(tx, "master", state.SchemaTaskRejoin); err != nil {
			return err
		}
		epoch = p.NodeEpoch(nodeID) + 1
		if p.Failover.Rejoining == nil {
			p.Failover.Rejoining = map[string]uint64{}
		}
		p.Failover.Rejoining[nodeID] = epoch
		return r.save(tx, p)
	}))
	return
}

func (r *Repository) FinishNodeRejoin(id int64, generation uint64, nodeID string, epoch uint64) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		if !p.NodeRetired(nodeID) && p.NodeEpoch(nodeID) == epoch {
			return errUnchanged
		}
		if !p.NodeRetired(nodeID) || p.Failover.Rejoining[nodeID] != epoch || epoch == 0 || len(p.Failover.Migrations) > 0 || p.Failover.ScannerSeeding || p.BudgetRevision == math.MaxUint64 {
			return ErrState
		}
		if p.Failover.NodeEpochs == nil {
			p.Failover.NodeEpochs = map[string]uint64{}
		}
		p.Failover.NodeEpochs[nodeID] = epoch
		delete(p.Failover.Retired, nodeID)
		delete(p.Failover.Rejoining, nodeID)
		if p.Failover.Rebalance == nil {
			p.Failover.Rebalance = &RebalanceState{}
		}
		if p.Failover.Rebalance.Targets == nil {
			p.Failover.Rebalance.Targets = map[string]bool{}
		}
		p.Failover.Rebalance.Targets[nodeID] = true
		if p.Failover.RebalancedEpochs == nil {
			p.Failover.RebalancedEpochs = map[string]uint64{}
		}
		p.Failover.RebalancedEpochs[nodeID] = epoch
		p.Failover.Rebalance.After, p.Failover.Rebalance.Pass = 0, 0
		p.BudgetRevision++
		return r.save(tx, p)
	}))
	return
}
