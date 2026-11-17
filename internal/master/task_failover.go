package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// taskStorageProof is how takeover recognizes the original shared directory:
// a marker written at failover setup or, for an upload source that no node
// could write to, the fingerprints of sampled files (ReadOnly; none for a
// source in which a complete listing found no file).
type taskStorageProof struct {
	Probe        transferstore.SharedProbe         `json:"probe"`
	Ready        bool                              `json:"ready"`
	ReadOnly     bool                              `json:"readOnly,omitempty"`
	Fingerprints []transferstore.SharedFingerprint `json:"fingerprints,omitempty"`
}

func (t *taskRuntime) verifyTaskStorage(ctx context.Context, p task.Parent, nodes []task.Node, initialize bool) error {
	if p.Spec.Source.Kind != "LOCAL" && p.Spec.Destination.Kind != "LOCAL" {
		return nil
	}
	if p.Spec.SharedCheckSkipped {
		// Takeover resumes on whatever directory each node has mounted.
		return t.server.grantSkippedSharedCheck(ctx, p, nodes)
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
		// Any node may write an upload source's marker, as in the precheck.
		written := false
		for _, node := range nodes {
			err := t.server.agentCall(ctx, nodeSession(node), "shared_probe_ensure", true, request(node), nil)
			if err == nil {
				written = true
				break
			}
			if p.Spec.Source.Kind != "LOCAL" || !unwritableFailure(err) {
				return err
			}
		}
		if !written {
			// No node can write the upload source: keep sampled fingerprints
			// instead; later checks compare against them, not a fresh sample.
			sample, _, err := t.server.proveSharedFingerprints(ctx, nodes, p.ID, p.Generation)
			if err != nil {
				return err
			}
			proof.ReadOnly, proof.Fingerprints, proof.Ready = true, sample, true
			log.Printf("task %d generation %d: no node could write the takeover marker into the shared source (%s); takeover compares %d sampled files instead", p.ID, p.Generation, t.server.nodeLabels(nodes), len(sample))
			return t.server.store.Update(func(tx *state.Tx) error {
				if err := allowTaskMetadataWrite(tx, p.ID); err != nil {
					return err
				}
				return tx.Put("master-task-storage-proofs", key, proof)
			})
		}
	}
	if proof.ReadOnly {
		return t.verifyReadOnlyTaskStorage(ctx, p, nodes, key, proof)
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

// sampleView is what one node sees at one sampled path, in a form that is
// equal exactly when two nodes see the same thing: "" for the sampled
// content, otherwise the difference, with the new size and digest of an edit.
func sampleView(difference transferstore.SharedFingerprintDifference, found bool) string {
	switch {
	case !found:
		return ""
	case difference.Status == transferstore.SharedFingerprintChanged:
		return fmt.Sprintf("%s %d %s", difference.Status, difference.Size, difference.SHA256)
	}
	return difference.Status
}

// verifyReadOnlyTaskStorage compares nodes with the fingerprints sampled when
// takeover was set up. Nodes that see different data at a sampled path do not
// see the same storage and are refused. When every node sees the same change
// at the source (a file edited, removed or made unreadable, or files added to
// a source that had none) and at least two nodes agree, the sample follows the
// source: removed files are dropped, edited ones keep their new fingerprints
// and fresh files top it up, all compared on every node again. One node alone
// cannot tell such a change from another directory, and a source with no file
// left looks like an unmounted share, so both keep waiting.
func (t *taskRuntime) verifyReadOnlyTaskStorage(ctx context.Context, p task.Parent, nodes []task.Node, key string, proof taskStorageProof) error {
	ctx, cancel := context.WithTimeout(ctx, sharedFingerprintBudget)
	defer cancel()
	server := t.server
	checks := server.compareSharedFingerprints(ctx, nodes, p.ID, p.Generation, proof.Fingerprints)
	changed := false
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("cannot verify the original shared directory: %w", check.err)
		}
		changed = changed || len(check.differences) > 0
	}
	if !changed {
		return nil
	}
	// An empty sample has one pseudo path: whether a node finds any file.
	expected := proof.Fingerprints
	if len(expected) == 0 {
		expected = []transferstore.SharedFingerprint{{}}
	}
	views := make([]map[string]transferstore.SharedFingerprintDifference, len(checks))
	for i, check := range checks {
		views[i] = map[string]transferstore.SharedFingerprintDifference{}
		for _, difference := range check.differences {
			path := difference.Path
			if len(proof.Fingerprints) == 0 {
				path = ""
			}
			views[i][path] = difference
		}
	}
	var kept []transferstore.SharedFingerprint
	edited, removed := 0, 0
	for _, want := range expected {
		first, firstFound := views[0][want.Path]
		for i := 1; i < len(checks); i++ {
			other, otherFound := views[i][want.Path]
			if sampleView(first, firstFound) != sampleView(other, otherFound) {
				return t.sampleDisagreement(want.Path, len(proof.Fingerprints) == 0, nodes[0], first, firstFound, nodes[i], other, otherFound)
			}
		}
		if len(nodes) < 2 && firstFound {
			return singleNodeSampleChange(server.nodeLabel(nodes[0]), first, len(proof.Fingerprints) == 0)
		}
		switch {
		case !firstFound:
			kept = append(kept, want)
		case first.Status == transferstore.SharedFingerprintChanged:
			kept = append(kept, transferstore.SharedFingerprint{Path: want.Path, Size: first.Size, SHA256: first.SHA256})
			edited++
		case first.Status != transferstore.SharedFingerprintUnexpected:
			removed++
		}
	}
	sample := kept
	if len(sample) < transferstore.SharedFingerprintMaxFiles {
		fresh, err := server.sampleSharedFingerprints(ctx, nodes[0], server.fingerprintPortable(ctx, nodes))
		if err != nil {
			return fmt.Errorf("cannot verify the original shared directory: %w", fingerprintTimeout(ctx, err))
		}
		known := map[string]bool{}
		for _, fingerprint := range kept {
			known[fingerprint.Path] = true
		}
		for _, fingerprint := range fresh {
			if len(sample) < transferstore.SharedFingerprintMaxFiles && !known[fingerprint.Path] {
				sample = append(sample, fingerprint)
			}
		}
	}
	if len(sample) == 0 {
		return fmt.Errorf("cannot verify the original shared directory: none of the %d files sampled when takeover was set up is left, and no node finds a file to sample now; check that the shared directory is still mounted on %s", len(proof.Fingerprints), server.nodeLabels(nodes))
	}
	for _, check := range server.compareSharedFingerprints(ctx, nodes, p.ID, p.Generation, sample) {
		if !check.matched() {
			return fmt.Errorf("cannot verify the original shared directory: %w", fingerprintTimeout(ctx, server.fingerprintFailure(ctx, nodes[0], check)))
		}
	}
	log.Printf("task %d generation %d: sampled files of the shared source changed at the source (%d edited, %d removed or unreadable, of %d sampled); nodes %s all see the same change, and the sample now has %d files that match on every one of them", p.ID, p.Generation, edited, removed, len(proof.Fingerprints), server.nodeLabels(nodes), len(sample))
	proof.Fingerprints = sample
	return t.server.store.Update(func(tx *state.Tx) error {
		if err := allowTaskMetadataWrite(tx, p.ID); err != nil {
			return err
		}
		return tx.Put("master-task-storage-proofs", key, proof)
	})
}

// sampleDisagreement explains two nodes that see different things at one
// sampled path: direct evidence that they do not see the same storage.
func (t *taskRuntime) sampleDisagreement(path string, emptySample bool, a task.Node, aDiff transferstore.SharedFingerprintDifference, aFound bool, b task.Node, bDiff transferstore.SharedFingerprintDifference, bFound bool) error {
	first, second := t.server.nodeLabel(a), t.server.nodeLabel(b)
	if emptySample {
		// One node finds a file, the other finds none.
		finder, found, other := first, aDiff.Path, second
		if !aFound {
			finder, found, other = second, bDiff.Path, first
		}
		return fmt.Errorf("Node %s finds file %q in the shared directory, which had no file when takeover was set up, but node %s finds none, so they do not see the same storage", finder, found, other)
	}
	describe := func(difference transferstore.SharedFingerprintDifference, found bool) string {
		switch {
		case !found:
			return "matches the sample"
		case difference.Status == transferstore.SharedFingerprintChanged:
			return "has another size or another first or last 64 KiB"
		case difference.Status == transferstore.SharedFingerprintMissing:
			return "is missing"
		}
		return "cannot be read: " + difference.Message
	}
	return fmt.Errorf("Nodes %s and %s see different data at sampled file %q (on %s it %s, on %s it %s), so they do not see the same storage", first, second, path, first, describe(aDiff, aFound), second, describe(bDiff, bFound))
}

// singleNodeSampleChange explains why one node alone cannot accept a change
// to the sampled files.
func singleNodeSampleChange(node string, difference transferstore.SharedFingerprintDifference, emptySample bool) error {
	const wait = "with one node online, a change at the source cannot be told apart from a different directory, so the check is repeated until another task node is online and sees the same change"
	switch {
	case emptySample:
		return fmt.Errorf("cannot verify the original shared directory: it had no file when takeover was set up, but node %s now finds %q; %s", node, difference.Path, wait)
	case difference.Status == transferstore.SharedFingerprintChanged:
		return fmt.Errorf("cannot verify the original shared directory: sampled file %q has changed on node %s since takeover was set up; %s", difference.Path, node, wait)
	case difference.Status == transferstore.SharedFingerprintMissing:
		return fmt.Errorf("cannot verify the original shared directory: sampled file %q is missing on node %s; %s", difference.Path, node, wait)
	}
	return fmt.Errorf("cannot verify the original shared directory: node %s cannot read sampled file %q (%s); %s", node, difference.Path, difference.Message, wait)
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
		// Logged once, when the node is retired (not while maintenance
		// protects it), rather than on every retried step.
		if p.Spec.SharedCheckSkipped && p.NodeRetired(node.ID) {
			log.Printf("task %d generation %d: node %s is lost; its work moves to %s without a shared directory check, which the operator chose to skip", p.ID, p.Generation, t.server.nodeLabel(node), t.server.nodeLabels(candidates))
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
	p, err = t.moveRetiredScanner(p, available)
	if err == nil {
		p, err = t.rejoinAvailableNodes(ctx, p)
	}
	return p, err
}

// moveRetiredScanner hands scanning to a verified available member through the
// checkpoint takeover path once the scanner has been fenced.
func (t *taskRuntime) moveRetiredScanner(p task.Parent, available map[string]bool) (task.Parent, error) {
	if !p.NodeRetired(p.ScannerNode().ID) || !(p.Status == "RESUMING" || !(p.ScanComplete && p.DispatchedThrough >= p.ScannedBatches)) {
		return p, nil
	}
	for _, node := range p.PlacementNodes() {
		if available[node.ID] {
			return t.repo.BeginScannerFailover(p.ID, p.Generation, node.ID)
		}
	}
	return p, nil
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
