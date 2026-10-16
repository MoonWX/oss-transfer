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
}

func newTaskRuntime(s *Server) *taskRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskRuntime{repo: task.New(s.store), server: s, ctx: ctx, cancel: cancel, active: map[int64]*taskRun{}, sharedProofs: map[int64]string{}, rates: map[int64]taskRateCache{}}
}

// Start launches control-plane reconciliation. HTTP construction alone does not
// create background work, making lifecycle ownership explicit in both binaries
// and integration tests.
func (s *Server) Start() {
	t := s.tasks
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started || t.closed {
		return
	}
	t.started = true
	t.workers.Add(1)
	go t.loop()
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
			return task.Parent{}, fmt.Errorf("Agent %s is not online in this enrollment", node.Address)
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
		s.topologyMu.Lock()
		p, err = s.tasks.repo.Get(id)
		if err == nil {
			err = s.checkTaskEnrollments(p)
		}
		if err == nil {
			if task.Historical(p) && p.Legacy.NeedsReconciliation {
				var resolved task.Spec
				resolved, err = s.resolveLegacySource(ctx, p)
				if err == nil {
					p, err = s.tasks.repo.BeginLegacyRecovery(id, resolved)
				}
			} else {
				p, err = s.tasks.repo.Resume(id)
			}
		}
		s.topologyMu.Unlock()
	case "clear":
		return s.tasks.repo.Recycle(id, true)
	case "restore":
		return s.tasks.repo.Recycle(id, false)
	case "permanent-delete":
		p, err = s.tasks.repo.Get(id)
		if err != nil {
			return p, err
		}
		if !p.Deleted || !task.Ended(p.Status) {
			return p, task.ErrState
		}
		err = s.tasks.exclusive(ctx, id, func() error {
			for _, node := range p.Spec.Nodes {
				retired, err := s.agents.registry.IsRevoked(nodeSession(node))
				if err != nil {
					return err
				}
				if retired {
					// Enrollment credentials are permanently fenced. Its old
					// local history cannot be cleaned through a replacement
					// enrollment; retain that data and purge the Master record.
					continue
				}
				if err := s.agentCall(ctx, nodeSession(node), "parent_delete", true, agentservice.ParentRequest{ParentID: id, Generation: p.Generation}, nil); err != nil {
					return err
				}
			}
			var err error
			p, err = s.tasks.repo.Purge(id)
			return err
		})
		return p, err
	default:
		return p, errors.New("unsupported task operation")
	}
	if err != nil {
		return p, err
	}
	if p.Status == "STOPPING" || p.Status == "RESUMING" {
		err = s.tasks.exclusive(ctx, id, func() error { return s.tasks.step(ctx, id) })
	}
	current, readErr := s.tasks.repo.Get(id)
	if readErr != nil {
		return p, readErr
	}
	return current, err
}
func (s *Server) changeTaskBandwidth(ctx context.Context, id int64, mbps float64) (task.Parent, error) {
	p, err := s.tasks.repo.Get(id)
	if err != nil {
		return p, err
	}
	if mbps > 0 && math.Floor(mbps*125000) < float64(len(p.Spec.Nodes)) {
		return p, errors.New("bandwidth budget is too small for the selected Agents")
	}
	p, err = s.tasks.repo.SetBandwidth(id, mbps)
	if err != nil {
		return p, err
	}
	err = s.tasks.exclusive(ctx, id, func() error { return s.tasks.policies(ctx, p) })
	return p, err
}
func parentPolicy(p task.Parent, index int) ratelimit.ParentLimit {
	total := int64(math.Floor(p.Spec.BandwidthMbps * 125000))
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
	for i, node := range p.Spec.Nodes {
		if err := t.server.agentCall(ctx, nodeSession(node), "bandwidth_parent", true, parentPolicy(p, i), nil); err != nil {
			problems = append(problems, fmt.Errorf("Agent %s bandwidth: %w", node.Address, err))
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
	var workers sync.WaitGroup
	results := make(chan error, len(p.Spec.Nodes)+1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := t.server.agentCall(callCtx, nodeSession(p.Spec.Nodes[0]), "scan_stop", true, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}, nil)
		if err != nil {
			results <- fmt.Errorf("scanner stop: %w", err)
		}
	}()
	for i, node := range p.Spec.Nodes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			err := t.server.agentCall(callCtx, nodeSession(node), "parent_stop", true, agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation}, nil)
			if err != nil {
				results <- fmt.Errorf("Agent %s stop: %w", node.Address, err)
				return
			}
			err = t.server.agentCall(callCtx, nodeSession(node), "bandwidth_parent", true, parentPolicy(p, i), nil)
			if err != nil {
				results <- fmt.Errorf("Agent %s stopped budget: %w", node.Address, err)
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
	spec := discovery.Spec{InventoryID: p.Spec.InventoryID, ParentID: p.ID, Generation: p.Generation, Source: p.Spec.Source, Prefix: p.Spec.Prefix, SingleKey: p.Spec.ObjectKey, StartKey: p.Spec.StartKey, EndKey: p.Spec.EndKey}
	if p.Legacy != nil && p.Legacy.RecoveryStarted {
		spec.LegacyManifestID = p.Legacy.MigrationID
	}
	if spec.Source.Kind == "LOCAL" {
		spec.Source.Root = p.Spec.Nodes[0].LocalRoot
	}
	if p.Spec.ModifiedFrom != nil {
		spec.ModifiedFrom = *p.Spec.ModifiedFrom
	}
	if p.Spec.ModifiedUntil != nil {
		spec.ModifiedUntil = *p.Spec.ModifiedUntil
	}
	return spec
}
func (t *taskRuntime) step(ctx context.Context, id int64) error {
	p, err := t.repo.Get(id)
	if err != nil {
		return err
	}
	if p.Deleted || task.Ended(p.Status) {
		return nil
	}
	s := t.server
	scanner := nodeSession(p.Spec.Nodes[0])
	control := agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation, RetryFailed: true, InventorySource: p.Spec.InventoryID != ""}
	scanControl := agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}
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
	for _, node := range p.Spec.Nodes {
		if err := s.syncStorage(ctx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}, snapshot); err != nil {
			return err
		}
	}
	if err := t.policies(ctx, p); err != nil {
		return err
	}
	if ready, err := t.syncInventory(ctx, p); err != nil {
		return err
	} else if !ready {
		return nil
	}
	if err := t.ensureSharedSource(ctx, p); err != nil {
		return err
	}
	if p.Spec.Destination.Kind == "LOCAL" {
		for _, node := range p.Spec.Nodes {
			if err := s.agentCall(ctx, nodeSession(node), "local_prepare", true, agentservice.PathRequest{Path: node.LocalRoot}, nil); err != nil {
				return err
			}
		}
	}
	if p.Status == "RESUMING" {
		for _, node := range p.Spec.Nodes {
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
	}
	var scan discovery.State
	if err := s.agentCall(ctx, scanner, "scan_start", true, t.scanSpec(p), &scan); err != nil {
		return err
	}
	if err := t.repo.ScanProgress(p.ID, p.Generation, scan.TotalObjects, scan.TotalBytes, int(scan.LastBatch), scan.ScanComplete, scan.Error); err != nil {
		return err
	}
	if err := t.observe(ctx, p, 64); err != nil {
		return err
	}
	outstanding, err := t.repo.Outstanding(p.ID, 0, 256)
	if err != nil {
		return err
	}
	// Keep a bounded amount of queued work in front of Agents. The scanner may
	// continue writing its manifest on disk while execution applies backpressure.
	if len(outstanding) < 64*len(p.Spec.Nodes) {
		for range 16 {
			var page struct {
				Batch discovery.Batch `json:"batch"`
				Found bool            `json:"found"`
			}
			scanControl.Number = int64(p.DispatchedThrough)
			if err := s.agentCall(ctx, scanner, "scan_batch", false, scanControl, &page); err != nil {
				return err
			}
			if !page.Found {
				break
			}
			child, err := t.repo.Assign(p.ID, p.Generation, int(page.Batch.Number), page.Batch.Objects, page.Batch.Legacy)
			if err != nil {
				return err
			}
			if err := s.agentCall(ctx, nodeSession(child.Node), "child_accept", true, agentservice.ChildAcceptRequest{Job: child.Job, InventorySource: p.Spec.InventoryID != ""}, nil); err != nil {
				return err
			}
			p.DispatchedThrough = int(page.Batch.Number)
			scanControl.Number = page.Batch.Number
			if err := s.agentCall(ctx, scanner, "scan_ack", true, scanControl, nil); err != nil {
				return err
			}
		}
	}
	_, err = t.repo.FinishIfReady(p.ID, p.Generation)
	return err
}
func (t *taskRuntime) observe(ctx context.Context, p task.Parent, limit int) error {
	children, err := t.repo.Outstanding(p.ID, 0, limit)
	if err != nil {
		return err
	}
	for _, child := range children {
		var record execution.Record
		err := t.server.agentCall(ctx, nodeSession(child.Node), "child_get", false, map[string]string{"childId": child.Job.ChildID}, &record)
		if agentStatus(err, 404) {
			if p.Status != "RUNNING" {
				continue
			}
			if err = t.server.agentCall(ctx, nodeSession(child.Node), "child_accept", true, agentservice.ChildAcceptRequest{Job: child.Job, InventorySource: p.Spec.InventoryID != ""}, nil); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := t.repo.Observe(p.ID, p.Generation, child.Job.Number, record); err != nil {
			return err
		}
	}
	return nil
}

// Page through the Agent ledger after every stop fence has been acknowledged.
// This avoids dropping final counters when more than one page of children exists.
func (t *taskRuntime) observeStopped(ctx context.Context, p task.Parent) error {
	for _, node := range p.Spec.Nodes {
		after := ""
		for {
			var rows []execution.Record
			if err := t.server.agentCall(ctx, nodeSession(node), "children", false, agentservice.ChildrenRequest{ParentID: p.ID, After: after, Limit: 8}, &rows); err != nil {
				return err
			}
			for _, record := range rows {
				if record.Job.Generation != p.Generation {
					return task.ErrGeneration
				}
				if err := t.repo.Observe(p.ID, p.Generation, record.Job.Number, record); err != nil {
					return err
				}
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
	for _, node := range p.Spec.Nodes {
		if enrolled[node.ID] != node.EnrollmentID {
			return fmt.Errorf("Agent %s has been removed or re-enrolled; the original task cannot resume on that identity", node.Address)
		}
	}
	return nil
}
