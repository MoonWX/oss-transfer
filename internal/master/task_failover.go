package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type taskStorageProof struct {
	Probe transferstore.SharedProbe `json:"probe"`
	Ready bool                      `json:"ready"`
}

func (t *taskRuntime) verifyTaskStorage(ctx context.Context, p task.Parent, nodes []task.Node, initialize bool) error {
	if p.Spec.Source.Kind != "LOCAL" && p.Spec.Destination.Kind != "LOCAL" {
		return nil
	}
	key := fmt.Sprint(p.ID)
	var proof taskStorageProof
	if err := t.server.store.Update(func(tx *state.Tx) error {
		if err := allowTaskMetadataWrite(tx, p.ID); err != nil {
			return err
		}
		found, err := tx.Get("master-task-storage-proofs", key, &proof)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		if !initialize {
			return errors.New("the original shared directory has not been verified")
		}
		proof.Probe, err = transferstore.NewSharedProbe()
		if err != nil {
			return err
		}
		return tx.Put("master-task-storage-proofs", key, proof)
	}); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("no shared directory verification node is available")
	}
	request := func(node task.Node) map[string]any {
		return map[string]any{"parentId": p.ID, "generation": p.Generation, "root": node.LocalRoot, "probe": proof.Probe}
	}
	if !proof.Ready {
		if !initialize || len(nodes) != len(p.ActiveNodes()) {
			return errors.New("waiting for every task node to verify the shared directory")
		}
		// This is the only creation site. Once sealed, a missing marker is a failed
		// mount check; it must never be recreated on an isolated replacement volume.
		if err := t.server.agentCall(ctx, nodeSession(nodes[0]), "shared_probe_ensure", true, request(nodes[0]), nil); err != nil {
			return err
		}
	}
	for _, node := range nodes {
		if err := t.server.agentCall(ctx, nodeSession(node), "shared_probe_verify", true, request(node), nil); err != nil {
			name := t.server.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
			return fmt.Errorf("Agent %s cannot verify the original shared directory: %w", name, err)
		}
	}
	if proof.Ready {
		return nil
	}
	proof.Ready = true
	return t.server.store.Update(func(tx *state.Tx) error {
		if err := allowTaskMetadataWrite(tx, p.ID); err != nil {
			return err
		}
		return tx.Put("master-task-storage-proofs", key, proof)
	})
}

func (t *taskRuntime) enableFailover(ctx context.Context, p task.Parent, available map[string]bool) (task.Parent, error) {
	if p.Failover != nil && p.Failover.Enabled {
		return p, nil
	}
	if len(p.Spec.Nodes) < 2 || p.Legacy != nil || p.Spec.Source.Kind == "LOCAL" && !p.Spec.SharedLocalSource {
		return p, nil
	}
	// An old/unreachable node may still execute without a lease. Keep the old
	// conservative behavior until every original participant supports fencing.
	for _, node := range p.ActiveNodes() {
		if !available[node.ID] {
			return p, nil
		}
		var caps struct {
			LeaseProtocol int `json:"leaseProtocol"`
		}
		if err := t.server.agentCall(ctx, nodeSession(node), "capabilities", false, nil, &caps); err != nil {
			return p, nil
		}
		if caps.LeaseProtocol < 1 {
			return p, nil
		}
	}
	if err := t.verifyTaskStorage(ctx, p, p.ActiveNodes(), true); err != nil {
		return p, err
	}
	return t.repo.EnableFailover(p.ID, p.Generation, p.Spec.Destination.Kind == "LOCAL")
}

func (t *taskRuntime) reconcileFailover(ctx context.Context, p task.Parent, available map[string]bool) (task.Parent, error) {
	var err error
	p, err = t.enableFailover(ctx, p, available)
	if err != nil {
		return p, err
	}
	if p.Failover == nil || !p.Failover.Enabled {
		return p, nil
	}
	// Arm all reachable owners before accepting any new work. A renewal loop
	// keeps these grants alive independently of expensive task reconciliation.
	for _, node := range p.ActiveNodes() {
		if !available[node.ID] {
			continue
		}
		if err := t.leases.renew(ctx, p, node); err != nil {
			delete(available, node.ID)
		}
	}
	var verified []task.Node
	for _, node := range p.ActiveNodes() {
		if available[node.ID] {
			verified = append(verified, node)
		}
	}
	if len(verified) > 0 {
		if err := t.verifyTaskStorage(ctx, p, verified, false); err != nil {
			return p, err
		}
	}
	for _, node := range p.ActiveNodes() {
		if available[node.ID] {
			continue
		}
		protected, err := t.server.agentUnderMaintenance(node.ID)
		if err != nil {
			return p, err
		}
		if protected {
			continue
		}
		expired, err := t.leases.expired(p, node)
		if err != nil {
			return p, err
		}
		if !expired {
			continue
		}
		candidates := []task.Node{}
		for _, candidate := range p.ActiveNodes() {
			if candidate.ID != node.ID && available[candidate.ID] {
				candidates = append(candidates, candidate)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		if err = t.verifyTaskStorage(ctx, p, candidates, false); err != nil {
			return p, err
		}
		p, err = t.retireUnavailableNode(p, node)
		if err != nil {
			return p, err
		}
	}
	// A lease can have been retired immediately before a Master crash, before
	// BeginFailover committed. Reconciliation above finishes that durable intent.
	for retired := range p.Failover.Migrations {
		_, _, err = t.repo.PrepareFailover(p.ID, p.Generation, retired, available)
		if err != nil {
			return p, err
		}
	}
	p, err = t.repo.Get(p.ID)
	if err != nil {
		return p, err
	}
	if p.NodeRetired(p.ScannerNode().ID) && (p.Status == "RESUMING" || !(p.ScanComplete && p.DispatchedThrough >= p.ScannedBatches)) {
		for _, node := range p.ActiveNodes() {
			if available[node.ID] {
				p, err = t.repo.BeginScannerFailover(p.ID, p.Generation, node.ID)
				break
			}
		}
	}
	if err == nil {
		p, err = t.rejoinAvailableNodes(ctx, p)
	}
	return p, err
}

func (t *taskRuntime) seedTakeoverScanner(ctx context.Context, p task.Parent) (bool, error) {
	if p.Failover == nil || !p.Failover.ScannerSeeding {
		return true, nil
	}
	nodes := []task.Node{p.ScannerNode()}
	if err := t.verifyTaskStorage(ctx, p, nodes, false); err != nil {
		return false, err
	}
	if ready, err := t.syncInventory(ctx, p); err != nil || !ready {
		return false, err
	}
	request := discovery.TakeoverRequest{Checkpoint: p.ScanCheckpoint, Spec: t.scanSpec(p), Epoch: p.Failover.ScannerEpoch, ThroughBatch: int64(p.Failover.ScannerThrough), Objects: p.DispatchedObjects, Bytes: p.DispatchedBytes}
	children, err := t.repo.Children(p.ID, p.Failover.ScannerSeedAfter, 16)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		if child.Job.Number > p.Failover.ScannerThrough {
			break
		}
		batch := discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: int64(child.Job.Number)}
		for _, item := range child.Job.Items {
			batch.Objects = append(batch.Objects, item.Source)
			batch.Bytes += item.Source.Size
		}
		request.Batches = append(request.Batches, batch)
		data, _ := json.Marshal(request)
		if len(data) > 3*1024*1024 && len(request.Batches) > 1 {
			request.Batches = request.Batches[:len(request.Batches)-1]
			break
		}
	}
	var scan discovery.State
	if err = t.server.agentCall(ctx, nodeSession(p.ScannerNode()), "scan_takeover_seed", true, request, &scan); err != nil {
		return false, err
	}
	if scan.Takeover == nil || scan.Takeover.Cleaning {
		return false, nil
	}
	after := int(scan.Takeover.SeededThrough)
	if after > p.Failover.ScannerThrough {
		return false, task.ErrConflict
	}
	if p, err = t.repo.AdvanceScannerSeed(p.ID, p.Generation, p.Failover.ScannerEpoch, after); err != nil {
		return false, err
	}
	if after < p.Failover.ScannerThrough {
		return false, nil
	}
	if err = t.server.agentCall(ctx, nodeSession(p.ScannerNode()), "scan_takeover_finalize", true, map[string]any{"parentId": p.ID, "generation": p.Generation, "epoch": p.Failover.ScannerEpoch}, &scan); err != nil {
		return false, err
	}
	_, err = t.repo.FinishScannerSeed(p.ID, p.Generation, p.Failover.ScannerEpoch)
	return err == nil, err
}

func childAcceptance(p task.Parent, child task.Child) agentservice.ChildAcceptRequest {
	request := agentservice.ChildAcceptRequest{Job: child.Job, InventorySource: p.Spec.InventoryID != ""}
	if child.Job.Assignment > 0 {
		request.Seed = child.Record
		if request.Seed == nil {
			request.Seed = &execution.Record{Job: child.Job, CreatedAt: time.Now().UTC()}
		}
	}
	return request
}
