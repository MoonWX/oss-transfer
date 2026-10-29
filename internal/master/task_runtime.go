package master

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type taskRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type taskRuntime struct {
	repo         *task.Repository
	server       *Server
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	active       map[int64]*taskRun
	workers      sync.WaitGroup
	started      bool
	closed       bool
	sharedProofs map[int64]string
	rates        map[int64]taskRateCache
	observeAfter map[int64]int
	leases       *taskLeases
	cleanup      *taskCleanupRuntime
}

func newTaskRuntime(s *Server) *taskRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	t := &taskRuntime{repo: task.New(s.store), server: s, ctx: ctx, cancel: cancel, active: map[int64]*taskRun{}, sharedProofs: map[int64]string{}, rates: map[int64]taskRateCache{}, observeAfter: map[int64]int{}}
	t.leases = newTaskLeases(t)
	t.cleanup = newTaskCleanupRuntime(t)
	return t
}

// Start launches control-plane reconciliation. HTTP construction alone does not
// create background work, making lifecycle ownership explicit in both binaries
// and integration tests.
func (s *Server) Start() {
	s.startSoftwareUpdates()
	t := s.tasks
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started || t.closed {
		return
	}
	t.started = true
	t.workers.Add(1)
	go t.loop()
	t.workers.Add(1)
	go func() { defer t.workers.Done(); t.leases.loop(t.ctx) }()
	t.workers.Add(1)
	go t.cleanup.loop()
}
func (t *taskRuntime) close() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	t.mu.Unlock()
	t.workers.Wait()
}
func (t *taskRuntime) loop() {
	defer t.workers.Done()
	timer := time.NewTicker(250 * time.Millisecond)
	defer timer.Stop()
	var after int64
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-timer.C:
		}
		parents, err := t.repo.List(after, 32, true)
		if err != nil {
			continue
		}
		if len(parents) < 32 {
			after = 0
		} else {
			after = parents[len(parents)-1].ID
		}
		for _, p := range parents {
			t.mu.Lock()
			if t.closed {
				t.mu.Unlock()
				return
			}
			if t.active[p.ID] != nil || len(t.active) >= 8 {
				t.mu.Unlock()
				continue
			}
			ctx, cancel := context.WithTimeout(t.ctx, 20*time.Second)
			run := &taskRun{cancel: cancel, done: make(chan struct{})}
			t.active[p.ID] = run
			t.workers.Add(1)
			t.mu.Unlock()
			go func() {
				defer t.workers.Done()
				defer cancel()
				defer func() { t.mu.Lock(); delete(t.active, p.ID); close(run.done); t.mu.Unlock() }()
				err := t.step(ctx, p.ID)
				if ctx.Err() == nil {
					message := ""
					if err != nil {
						message = err.Error()
					}
					_ = t.repo.RuntimeProblem(p.ID, p.Generation, message)
				}
			}()
		}
	}
}
func (t *taskRuntime) exclusive(ctx context.Context, id int64, fn func() error) error {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return errors.New("Master is stopping")
		}
		old := t.active[id]
		if old == nil {
			run := &taskRun{cancel: func() {}, done: make(chan struct{})}
			t.active[id] = run
			t.workers.Add(1)
			t.mu.Unlock()
			defer t.workers.Done()
			defer func() { t.mu.Lock(); delete(t.active, id); close(run.done); t.mu.Unlock() }()
			return fn()
		}
		old.cancel()
		t.mu.Unlock()
		select {
		case <-old.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func nodeSession(node task.Node) agenttransport.Session {
	return agenttransport.Session{NodeID: node.ID, EnrollmentID: node.EnrollmentID}
}
func agentStatus(err error, status int) bool {
	var e *AgentError
	return errors.As(err, &e) && e.Status == status
}

func (s *Server) createTransfer(ctx context.Context, spec task.Spec, requestKey string, checks ...func(*state.Tx) error) (task.Parent, error) {
	s.topologyMu.Lock()
	defer s.topologyMu.Unlock()
	online, err := s.onlineAgents()
	if err != nil {
		return task.Parent{}, err
	}
	members := map[string]cluster.Member{}
	for _, member := range online {
		members[member.ID] = member
	}
	for i, node := range spec.Nodes {
		member, ok := members[node.ID]
		if !ok || member.EnrollmentID != node.EnrollmentID {
			name := s.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
			return task.Parent{}, fmt.Errorf("Agent %s is not online in this enrollment", name)
		}
		spec.Nodes[i].Address = member.Address
	}
	if spec.Direction == "LOCAL_TO_OSS" && len(spec.Nodes) > 1 {
		spec.DistributionMode = "BALANCED"
		spec.SharedLocalSource = true
	}
	if spec.BandwidthMbps > 0 && math.Floor(spec.BandwidthMbps*125000) < float64(len(spec.Nodes)) {
		return task.Parent{}, errors.New("bandwidth budget is too small for the selected Agents")
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		return task.Parent{}, err
	}
	for _, endpoint := range []execution.Endpoint{spec.Source, spec.Destination} {
		if endpoint.Kind == "S3" {
			found := false
			for _, config := range snapshot.Connections {
				if config.Alias == endpoint.Ref {
					if _, err := config.S3Config(endpoint.Bucket); err != nil {
						return task.Parent{}, err
					}
					found = true
					break
				}
			}
			if !found {
				return task.Parent{}, fmt.Errorf("storage connection %s does not exist", endpoint.Ref)
			}
		}
	}
	p, _, err := s.tasks.repo.CreateChecked(spec, requestKey, func(tx *state.Tx) error {
		for _, check := range checks {
			if err := check(tx); err != nil {
				return err
			}
		}
		return nil
	})
	return p, err
}
func (s *Server) controlTask(ctx context.Context, id int64, action string) (task.Parent, error) {
	var p task.Parent
	var err error
	switch action {
	case "stop":
		p, err = s.tasks.repo.Stop(id)
	case "resume", "retry":
		p, _, err = s.requestTaskResume(ctx, id, nil)
	case "clear":
		return s.tasks.repo.Recycle(id, true)
	case "restore":
		return s.tasks.repo.Recycle(id, false)
	case "permanent-delete":
		p, _, err = s.requestTaskPurge(ctx, id)
		return p, err
	default:
		return p, errors.New("unsupported task operation")
	}
	if err != nil {
		return p, err
	}
	if p.Status == "STOPPING" {
		err = s.tasks.exclusive(ctx, id, func() error { return s.tasks.step(ctx, id) })
	}
	current, readErr := s.tasks.repo.Get(id)
	if readErr != nil {
		return p, readErr
	}
	return current, err
}

// requestTaskResume serializes membership validation with task admission; the
// repository compares the expected generation inside the durable transaction.
func (s *Server) requestTaskResume(ctx context.Context, id int64, expected *uint64) (task.Parent, bool, error) {
	s.topologyMu.Lock()
	defer s.topologyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Parent{}, false, err
	}
	p, err := s.tasks.repo.Get(id)
	if err != nil {
		return p, false, err
	}
	legacyRecovery := task.Historical(p) && p.Legacy.NeedsReconciliation
	if p.Deleted || p.Purged || (expected != nil && p.Generation != *expected) ||
		p.Status == "RESUMING" || p.Status == "RUNNING" || p.Status == "LIST_DONE" || (p.Status == "COMPLETED" && !legacyRecovery) {
		return s.tasks.repo.RequestResume(id, expected)
	}
	if err = s.checkTaskEnrollments(p); err != nil {
		return p, false, err
	}
	if legacyRecovery {
		resolved, err := s.resolveLegacySource(ctx, p)
		if err != nil {
			return p, false, err
		}
		return s.tasks.repo.RequestLegacyRecovery(id, resolved, expected)
	}
	return s.tasks.repo.RequestResume(id, expected)
}

func (s *Server) changeTaskBandwidth(ctx context.Context, id int64, mbps float64) (task.Parent, error) {
	return s.changeTaskLimits(ctx, id, &mbps, nil, false, nil)
}
func (s *Server) changeTaskLimits(ctx context.Context, id int64, mbps *float64, limits *execution.RequestLimits, replaceRequests bool, patch *task.RequestLimitsPatch) (p task.Parent, err error) {
	// Serialize persistence and delivery with task lifecycle operations. A late
	// delivery must not restore limits from before a concurrent edit or stop.
	err = s.tasks.exclusive(ctx, id, func() error {
		var updateErr error
		p, updateErr = s.tasks.repo.UpdateLimits(id, mbps, limits, replaceRequests, patch)
		if updateErr != nil {
			return updateErr
		}
		return s.tasks.policies(ctx, p)
	})
	return
}
func parentPolicy(p task.Parent, index int) ratelimit.ParentLimit {
	total := int64(math.Floor(p.Spec.BandwidthMbps * 125000))
	p.Spec.Nodes = p.ActiveNodes()
	members := int64(len(p.Spec.Nodes))
	rate := int64(0)
	if total > 0 {
		rate = total / members
		if int64(index) < total%members {
			rate++
		}
	}
	return task.ApplyRequestLimits(ratelimit.ParentLimit{ParentID: p.ID, Generation: p.Generation, Revision: p.BudgetRevision, BandwidthBytesPerSecond: rate, Stopped: p.Status == "STOPPING" || p.Status == "STOPPED"}, p.Spec)
}
func (t *taskRuntime) policies(ctx context.Context, p task.Parent) error {
	// Offline members keep their reserved share until their old attempts are fenced.
	var problems []error
	var displays nodeDisplaySet
	for i, node := range p.ActiveNodes() {
		if err := t.server.agentCall(ctx, nodeSession(node), "bandwidth_parent", true, parentPolicy(p, i), nil); err != nil {
			if len(problems) == 0 {
				displays = t.server.nodeDisplaySnapshot()
			}
			problems = append(problems, fmt.Errorf("Agent %s bandwidth: %w", displays.lookup(node.ID, node.Address).DisplayLabel, err))
		}
	}
	return errors.Join(problems...)
}

// Stop every execution member even when the scanner or another member is offline.
// A missing acknowledgement keeps the durable parent in STOPPING for reconciliation.
func (t *taskRuntime) stopMembers(ctx context.Context, p task.Parent) error {
	t.mu.Lock()
	delete(t.sharedProofs, p.ID)
	t.mu.Unlock()
	displays := t.server.nodeDisplaySnapshot()
	var workers sync.WaitGroup
	results := make(chan error, len(p.Spec.Nodes)+1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		stopped, err := t.nodeLeaseStopped(p, p.ScannerNode())
		if err != nil {
			results <- err
			return
		}
		if stopped {
			return
		}
		err = t.server.agentCall(callCtx, nodeSession(p.ScannerNode()), "scan_stop", true, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation, Epoch: p.ScannerEpoch()}, nil)
		if err != nil {
			results <- fmt.Errorf("scanner stop: %w", err)
		}
	}()
	for i, node := range p.ActiveNodes() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			stopped, err := t.nodeLeaseStopped(p, node)
			if err != nil {
				results <- err
				return
			}
			if stopped {
				return
			}
			err = t.server.agentCall(callCtx, nodeSession(node), "parent_stop", true, agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation}, nil)
			if err != nil {
				results <- fmt.Errorf("Agent %s stop: %w", displays.lookup(node.ID, node.Address).DisplayLabel, err)
				return
			}
			err = t.server.agentCall(callCtx, nodeSession(node), "bandwidth_parent", true, parentPolicy(p, i), nil)
			if err != nil {
				results <- fmt.Errorf("Agent %s stopped budget: %w", displays.lookup(node.ID, node.Address).DisplayLabel, err)
			}
		}()
	}
	workers.Wait()
	close(results)
	var problems []error
	for err := range results {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}
func (t *taskRuntime) scanSpec(p task.Parent) discovery.Spec {
	spec := discovery.Spec{InventoryID: p.Spec.InventoryID, ParentID: p.ID, Generation: p.Generation, Epoch: p.ScannerEpoch(), Source: p.Spec.Source, Prefix: p.Spec.Prefix, SingleKey: p.Spec.ObjectKey, StartKey: p.Spec.StartKey, EndKey: p.Spec.EndKey}
	if p.Legacy != nil && p.Legacy.RecoveryStarted {
		spec.LegacyManifestID = p.Legacy.MigrationID
	}
	if spec.Source.Kind == "LOCAL" {
		spec.Source.Root = p.ScannerNode().LocalRoot
	}
	if p.Spec.ModifiedFrom != nil {
		spec.ModifiedFrom = *p.Spec.ModifiedFrom
	}
	if p.Spec.ModifiedUntil != nil {
		spec.ModifiedUntil = *p.Spec.ModifiedUntil
	}
	return spec
}

// Prepare peers independently: one unavailable Agent must not consume the
// entire reconciliation deadline before healthy peers can report or accept work.
func (t *taskRuntime) prepareMembers(ctx context.Context, p task.Parent, snapshot storageconfig.Snapshot) (map[string]bool, error) {
	available := map[string]bool{}
	var mu sync.Mutex
	var workers sync.WaitGroup
	var problems []error
	slots := make(chan struct{}, 8)
	displays := t.server.nodeDisplaySnapshot()
	for i, node := range p.ActiveNodes() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if t.server.softwareUpdateBlocksNode(node.ID) {
				mu.Lock()
				problems = append(problems, fmt.Errorf("Agent %s is updating; its assigned work remains on that node", displays.lookup(node.ID, node.Address).DisplayLabel))
				mu.Unlock()
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			err := t.server.syncStorage(callCtx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}, snapshot)
			if err == nil {
				// Keep the original member index/count so an isolated peer keeps
				// its reserved budget rather than accidentally exceeding a limit.
				err = t.server.agentCall(callCtx, nodeSession(node), "bandwidth_parent", true, parentPolicy(p, i), nil)
			}
			if err == nil && p.Spec.Destination.Kind == "LOCAL" {
				err = t.server.agentCall(callCtx, nodeSession(node), "local_prepare", true, agentservice.PathRequest{Path: node.LocalRoot}, nil)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				problems = append(problems, fmt.Errorf("Agent %s is unavailable; its assigned work remains on that node: %w", displays.lookup(node.ID, node.Address).DisplayLabel, err))
			} else {
				available[node.ID] = true
			}
		}()
	}
	workers.Wait()
	return available, errors.Join(problems...)
}

func (t *taskRuntime) step(ctx context.Context, id int64) (resultErr error) {
	p, err := t.repo.Get(id)
	if err != nil {
		return err
	}
	if p.Deleted || task.Ended(p.Status) {
		return nil
	}
	if p.Status == "RESUMING" && (p.ActiveTime == nil || (p.ResumeAfter == nil && p.Scheduling == nil)) {
		p, err = t.repo.Resume(p.ID)
		if err != nil {
			return err
		}
	}
	if p.Status == "RESUMING" && p.ResumeAfter != nil {
		ready := false
		for pages := 0; pages < 16 && !ready; pages++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			ready, err = t.repo.PrepareResume(p.ID, p.Generation)
			if err != nil {
				return err
			}
		}
		if !ready {
			return nil
		}
		p, err = t.repo.Get(id)
		if err != nil {
			return err
		}
	}
	s := t.server
	scanner := nodeSession(p.ScannerNode())
	control := agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation, RetryFailed: true, InventorySource: p.Spec.InventoryID != ""}
	scanControl := agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation, Epoch: p.ScannerEpoch()}
	if p.Status == "STOPPING" {
		if err := t.stopMembers(ctx, p); err != nil {
			return err
		}
		if err := t.observeStopped(ctx, p); err != nil {
			return err
		}
		return t.repo.Stopped(p.ID, p.Generation)
	}
	defer t.sampleRates(ctx, p)
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		return err
	}
	available, memberErr := t.prepareMembers(ctx, p, snapshot)
	p, err = t.reconcileFailover(ctx, p, available)
	if err != nil {
		return err
	}
	scanner = nodeSession(p.ScannerNode())
	scanControl.Epoch = p.ScannerEpoch()
	if p.Failover != nil && p.Failover.Enabled {
		// Retired peers are no longer required for progress or control ACKs.
		memberErr = nil
		for _, node := range p.ActiveNodes() {
			if !available[node.ID] {
				memberErr = errors.New("Waiting for an unavailable node's execution lease to expire")
			}
		}
	}
	defer func() { resultErr = errors.Join(memberErr, resultErr) }()
	// Observations do not require the scanner. Preserve healthy peers' progress
	// even when the original scanner is down or a shared-mount check must wait.
	if p.Status == "RUNNING" {
		if err := t.observeAvailable(ctx, p, 64, available); err != nil {
			memberErr = errors.Join(memberErr, err)
		}
	}
	if p.Failover != nil && p.Failover.ScannerSeeding {
		ready, seedErr := t.seedTakeoverScanner(ctx, p)
		if seedErr != nil {
			return seedErr
		}
		if !ready {
			return nil
		}
		p, err = t.repo.Get(p.ID)
		if err != nil {
			return err
		}
	}
	if !available[p.ScannerNode().ID] {
		// A completed persisted manifest and confirmed child outcomes are
		// sufficient to finish; the scanner need not reconnect just to repeat them.
		finished, err := t.repo.FinishIfReady(p.ID, p.Generation)
		if err != nil {
			return err
		}
		if finished {
			memberErr = nil
			return nil
		}
		return errors.New("Waiting for the scanning node to reconnect; existing child tasks remain assigned to their original nodes")
	}
	if p.Status == "RESUMING" && len(available) != len(p.ActiveNodes()) {
		return errors.New("Waiting for all task nodes to acknowledge recovery")
	}
	if ready, err := t.syncInventory(ctx, p); err != nil {
		return err
	} else if !ready {
		return nil
	}
	proofParent := p
	proofParent.Spec.Nodes = nil
	for _, node := range p.ActiveNodes() {
		if available[node.ID] {
			proofParent.Spec.Nodes = append(proofParent.Spec.Nodes, node)
		}
	}
	if p.Failover != nil && p.Failover.Enabled {
		if err := t.verifyTaskStorage(ctx, p, proofParent.Spec.Nodes, false); err != nil {
			return err
		}
	} else if err := t.ensureSharedSource(ctx, proofParent); err != nil {
		return err
	}
	if p.Status == "RESUMING" {
		for _, node := range p.ActiveNodes() {
			err := s.agentCall(ctx, nodeSession(node), "parent_resume", true, control, nil)
			if agentStatus(err, 404) {
				old := control
				old.Generation--
				if err = s.agentCall(ctx, nodeSession(node), "parent_stop", true, old, nil); err == nil {
					err = s.agentCall(ctx, nodeSession(node), "parent_resume", true, control, nil)
				}
			}
			if err != nil {
				return err
			}
		}
		err := s.agentCall(ctx, scanner, "scan_resume", true, scanControl, nil)
		if agentStatus(err, 404) {
			err = s.agentCall(ctx, scanner, "scan_start", true, t.scanSpec(p), nil)
		}
		if err != nil {
			return err
		}
		if err := t.repo.Resumed(p.ID, p.Generation); err != nil {
			return err
		}
		p.Status = "RUNNING"
		if err := t.observeAvailable(ctx, p, 64, available); err != nil {
			memberErr = errors.Join(memberErr, err)
		}
	}
	var scan discovery.State
	if err := s.agentCall(ctx, scanner, "scan_start", true, t.scanSpec(p), &scan); err != nil {
		return err
	}
	if err := t.repo.ScanProgressForScanner(p.ID, p.Generation, p.ScannerEpoch(), scan.TotalObjects, scan.TotalBytes, int(scan.LastBatch), scan.ScanComplete, scan.Error); err != nil {
		return err
	}
	p, err = t.repo.Get(p.ID)
	if err != nil {
		return err
	}
	if err := t.reconcileScanAck(ctx, p, scan); err != nil {
		return err
	}
	// Keep a bounded amount of queued work in front of Agents. The scanner may
	// continue writing its manifest on disk while execution applies backpressure.
	if pendingDispatchCapacity(p) > 0 {
		if err := t.dispatchAvailable(ctx, p, scanControl, available); err != nil {
			return err
		}
	}
	finished, err := t.repo.FinishIfReady(p.ID, p.Generation)
	if finished {
		memberErr = nil
	}
	return err
}

// Read a small dispatch page, persist its assignments together, then deliver.
// The existing outstanding reconciliation retries any undelivered assignments.
func (t *taskRuntime) dispatch(ctx context.Context, p task.Parent, control agentservice.ScanRequest) error {
	return t.dispatchAvailable(ctx, p, control, nil)
}

func (t *taskRuntime) dispatchAvailable(ctx context.Context, p task.Parent, control agentservice.ScanRequest, available map[string]bool) error {
	scanner := nodeSession(p.ScannerNode())
	control.Number = int64(p.DispatchedThrough)
	limit := min(16, pendingDispatchCapacity(p))
	if limit <= 0 {
		return nil
	}
	if p.DispatchedThrough == 0 {
		limit = 1 // Start the first transfer without collecting the rest of a page.
	}
	batches, collectionErr := t.collectBatches(ctx, p, control, limit)
	if len(batches) > 0 {
		control.Number = int64(batches[len(batches)-1].Number)
	}
	if collectionErr == nil {
		collectionErr = ctx.Err()
	}
	if len(batches) == 0 {
		return collectionErr
	}
	// Retain the validated prefix even when a slow or unavailable scanner ends
	// this collection. Otherwise every retry could reread the same prefix and
	// never dispatch it. AssignPage still fences stopped/newer generations.
	var children []task.Child
	var err error
	if available == nil {
		available = map[string]bool{}
		for _, node := range p.ActiveNodes() {
			available[node.ID] = true
		}
	}
	children, err = t.repo.AssignPageForScanner(p.ID, p.Generation, p.ScannerEpoch(), batches, available)
	if err != nil {
		return errors.Join(collectionErr, err)
	}
	if collectionErr != nil {
		return collectionErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.deliverChildren(ctx, p, children); err != nil {
		return err
	}
	var acknowledged discovery.State
	if err := t.server.agentCall(ctx, scanner, "scan_ack", true, control, &acknowledged); err != nil {
		return err
	}
	return t.releaseScanPrefix(ctx, p, acknowledged)
}

func pendingDispatchCapacity(p task.Parent) int {
	// Counters cover the entire task; a 256-row query cannot enforce this
	// bound once five or more nodes have outstanding assignments.
	return max(0, 64*len(p.ActiveNodes())-(p.ChildCount-p.FinishedChildren))
}

func (t *taskRuntime) observe(ctx context.Context, p task.Parent, limit int) (err error) {
	return t.observeAvailable(ctx, p, limit, nil)
}

func (t *taskRuntime) observeAvailable(ctx context.Context, p task.Parent, limit int, available map[string]bool) (err error) {
	t.mu.Lock()
	after := t.observeAfter[p.ID]
	t.mu.Unlock()
	children, err := t.repo.Outstanding(p.ID, after, limit)
	if err == nil && len(children) == 0 && after > 0 {
		children, err = t.repo.Outstanding(p.ID, 0, limit)
	}
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	// Advance the whole bounded page, including unavailable peers. A slow
	// member must not keep the next reconciliation at the same prefix.
	t.mu.Lock()
	if t.observeAfter == nil {
		t.observeAfter = map[int64]int{}
	}
	t.observeAfter[p.ID] = children[len(children)-1].Job.Number
	t.mu.Unlock()
	groups := groupChildrenByNode(children)
	observed := make([][]task.Observation, len(groups))
	unavailable := make([]bool, len(groups))
	err = parallelChildNodes(ctx, groups, func(index int, children []task.Child) error {
		if available != nil && !available[children[0].Node.ID] {
			return nil
		}
		var nodeErr error
		observed[index], nodeErr = t.observeNode(ctx, p, children, available != nil)
		unavailable[index] = nodeErr != nil
		return nodeErr
	})
	observations := make([]task.Observation, 0, len(children))
	for index, group := range groups {
		observations = append(observations, observed[index]...)
		if available != nil && unavailable[index] {
			delete(available, group[0].Node.ID)
		}
	}
	// Persist healthy peers' reports even when a different peer times out.
	err = errors.Join(err, t.repo.ObserveBatch(p.ID, p.Generation, observations))
	return err
}

// Page through the Agent ledger after every stop fence has been acknowledged.
// This avoids dropping final counters when more than one page of children exists.
func (t *taskRuntime) observeStopped(ctx context.Context, p task.Parent) error {
	for _, node := range p.ActiveNodes() {
		stopped, err := t.nodeLeaseStopped(p, node)
		if err != nil {
			return err
		}
		if stopped {
			continue
		}
		after := ""
		for {
			var rows []execution.Record
			if err := t.server.agentCall(ctx, nodeSession(node), "children", false, agentservice.ChildrenRequest{ParentID: p.ID, After: after, Limit: 8}, &rows); err != nil {
				return err
			}
			observations := make([]task.Observation, 0, len(rows))
			for _, record := range rows {
				if record.Job.Generation != p.Generation {
					// Successful historical children are not rewritten for every retry.
					if record.Job.Generation < p.Generation && record.Status == execution.Completed {
						continue
					}
					return task.ErrGeneration
				}
				observations = append(observations, task.Observation{Number: record.Job.Number, Record: record})
			}
			if err := t.repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
				return err
			}
			if len(rows) < 8 {
				break
			}
			next := rows[len(rows)-1].Job.ChildID
			if next == after {
				return errors.New("Agent child pagination did not advance")
			}
			after = next
		}
	}
	return nil
}

func (s *Server) checkTaskEnrollments(p task.Parent) error {
	members, err := s.agents.registry.Members()
	if err != nil {
		return err
	}
	enrolled := map[string]string{}
	for _, m := range members {
		enrolled[m.ID] = m.EnrollmentID
	}
	for _, node := range p.ActiveNodes() {
		if enrolled[node.ID] != node.EnrollmentID {
			name := s.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
			return fmt.Errorf("Agent %s has been removed or re-enrolled; the original task cannot resume on that identity", name)
		}
	}
	return nil
}
