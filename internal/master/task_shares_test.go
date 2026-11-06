package master

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Agent behaviors for bandwidth_parent in share tests.
const (
	sharePass = iota
	shareFail // the Agent is unreachable: nothing is applied
	shareDrop // the Agent applies the policy, but its reply is lost
)

type shareEvent struct {
	node      string
	bandwidth int64
	revision  uint64
	done      bool
}

// shareRecorder logs every bandwidth_parent an Agent receives, in one order
// across all Agents, and decides how each Agent answers.
type shareRecorder struct {
	mu     sync.Mutex
	events []shareEvent
	modes  map[string]int
}

func (r *shareRecorder) mode(node string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modes[node]
}

func (r *shareRecorder) set(node string, mode int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.modes == nil {
		r.modes = map[string]int{}
	}
	r.modes[node] = mode
}

func (r *shareRecorder) log(e shareEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *shareRecorder) snapshot() []shareEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]shareEvent(nil), r.events...)
}

type shareExecutor struct {
	*agentservice.Service
	node     *string
	recorder *shareRecorder
}

func (e shareExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "capabilities":
		raw, _ := json.Marshal(e.LeaseCapabilities())
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	case "bandwidth_parent":
		var policy ratelimit.ParentLimit
		_ = json.Unmarshal(c.Payload, &policy)
		mode := e.recorder.mode(*e.node)
		if mode == shareFail {
			return agenttransport.Response{Status: 503, Payload: json.RawMessage(`{"message":"Agent is unreachable"}`)}, nil
		}
		e.recorder.log(shareEvent{node: *e.node, bandwidth: policy.BandwidthBytesPerSecond, revision: policy.Revision})
		response, err := e.Service.Execute(ctx, c)
		e.recorder.log(shareEvent{node: *e.node, bandwidth: policy.BandwidthBytesPerSecond, revision: policy.Revision, done: true})
		if mode == shareDrop {
			return agenttransport.Response{Status: 503, Payload: json.RawMessage(`{"message":"reply lost"}`)}, nil
		}
		return response, err
	}
	return e.Service.Execute(ctx, c)
}

func shareTestAgent(t *testing.T, f *agentHTTPFixture, name string, recorder *shareRecorder) *takeoverAgent {
	t.Helper()
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, name+".example"), name, credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &takeoverAgent{node: task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}, service: service, cancel: cancel, done: make(chan error, 1)}
	runner := agenttransport.NewRunner(agentClient(t, f, credential), shareExecutor{Service: service, node: &a.node.ID, recorder: recorder}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { a.done <- runner.Run(ctx) }()
	t.Cleanup(func() { a.cancel(); <-a.done; service.Close(context.Background()); db.Close() })
	return a
}

const shareTestTotal = int64(125_000_000) // 1000 Mbps

func shareTestFloor(nodes int) int64 { return shareTestTotal / int64(64*nodes) }

// shareTask creates a 1000 Mbps task over the agents whose scan is complete
// and whose only child runs on the first agent.
func shareTask(t *testing.T, f *agentHTTPFixture, agents ...*takeoverAgent) task.Parent {
	t.Helper()
	var nodes []task.Node
	for _, a := range agents {
		nodes = append(nodes, a.node)
	}
	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: nodes, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", BandwidthMbps: 1000,
		RequestLimits: &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 64}}, "shares")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.master.tasks.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 1, Objects: []transferstore.Object{{Key: "big", Size: 1 << 30}}}}, map[string]bool{agents[0].node.ID: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.ScanProgress(p.ID, p.Generation, 1, 1<<30, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	p, err = f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// shareStep models one reconciliation step: preparation delivers the current
// policy to every member, then the allocation passes run.
func shareStep(t *testing.T, ctx context.Context, runtime *taskRuntime, id int64, available map[string]bool) task.Parent {
	t.Helper()
	p, err := runtime.repo.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range p.ActiveNodes() {
		if available[node.ID] {
			_ = runtime.deliverPolicy(ctx, p, i, node)
		}
	}
	return runtime.reallocateShares(ctx, p, available)
}

func installedBandwidth(t *testing.T, a *takeoverAgent, id int64) int64 {
	t.Helper()
	return appliedBandwidth(t, a, id)
}

// The user-reported case: two nodes share 1000 Mbps, one drains its last big
// child while the other has nothing left. The idle node is reduced first; the
// busy node is raised only after that reduction has been acknowledged.
func TestIdleShareMovesToBusyNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "busy-a", recorder), shareTestAgent(t, f, "idle-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	floor := shareTestFloor(2)
	for range 4 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := installedBandwidth(t, b, p.ID); got != floor {
		t.Fatal("idle node keeps more than the floor", got, floor)
	}
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal-floor {
		t.Fatal("busy node did not receive the released share", got)
	}
	reducedAt, raisedAt := -1, -1
	for i, e := range recorder.snapshot() {
		if e.node == b.node.ID && e.done && e.bandwidth == floor && reducedAt < 0 {
			reducedAt = i
		}
		if e.node == a.node.ID && !e.done && e.bandwidth > shareTestTotal/2 && raisedAt < 0 {
			raisedAt = i
		}
	}
	if reducedAt < 0 || raisedAt < 0 || raisedAt < reducedAt {
		t.Fatal("raise was sent before the reduction was applied", reducedAt, raisedAt, recorder.snapshot())
	}
	// The List QPS dimension follows the same demand.
	policy, err := a.service.Limits.Parent(p.ID)
	if err != nil || policy.ListQPS <= 32 || policy.ListQPS > 64-64.0/128 {
		t.Fatal("list QPS not reallocated", policy, err)
	}
	// The real step places the allocation before anything that can end it.
	if err := runtime.step(ctx, p.ID); err == nil {
		t.Log("step completed")
	}
	current, _ := runtime.repo.Get(p.ID)
	if task.WireRevision(current) != task.WireRevision(p) {
		t.Fatal("a settled allocation committed again", task.WireRevision(current), task.WireRevision(p))
	}
}

// While the idle node cannot be reached, its reduction is not acknowledged,
// so the busy node is not raised; once it answers, the raise follows.
func TestRaiseWaitsForReductionAck(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "wait-a", recorder), shareTestAgent(t, f, "wait-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	recorder.set(b.node.ID, shareFail)
	for range 6 {
		p = shareStep(t, ctx, runtime, p.ID, available)
		if got := installedBandwidth(t, a, p.ID); got != shareTestTotal/2 {
			t.Fatal("busy node raised before the idle node acknowledged its reduction", got)
		}
	}
	if entry := p.Shares.Nodes[b.node.ID]; entry.Share.Bandwidth != shareTestFloor(2) || entry.Reserved.Bandwidth != shareTestTotal/2 {
		t.Fatal("the unacknowledged reduction must stay reserved", entry)
	}
	recorder.set(b.node.ID, sharePass)
	for range 2 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal-shareTestFloor(2) {
		t.Fatal("busy node not raised after the acknowledgement", got)
	}
}

// The idle node applied its reduction but the reply was lost: the Master
// cannot know, so it keeps the old share reserved until a repeated delivery is
// acknowledged.
func TestLostReductionAckKeepsReserved(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "lost-a", recorder), shareTestAgent(t, f, "lost-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	recorder.set(b.node.ID, shareDrop)
	for range 4 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := installedBandwidth(t, b, p.ID); got != shareTestFloor(2) {
		t.Fatal("the idle node did not apply its reduction", got)
	}
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal/2 {
		t.Fatal("raised although the reduction's acknowledgement was lost", got)
	}
	recorder.set(b.node.ID, sharePass)
	p = shareStep(t, ctx, runtime, p.ID, available)
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal-shareTestFloor(2) {
		t.Fatal("not raised after the repeated delivery was acknowledged", got)
	}
}

// A restarted Master forgets acknowledgements. A reduction committed before
// the restart stays reserved until the node acknowledges it again.
func TestMasterRestartDoesNotRaiseBeforeReAck(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "restart-a", recorder), shareTestAgent(t, f, "restart-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	idle := map[string]bool{b.node.ID: true}
	p, changed, err := runtime.repo.ReallocateShares(p.ID, p.Generation, task.WireRevision(p), runtime.shareAckedNodes(p), idle)
	if err != nil || !changed {
		t.Fatal("reduction not committed", changed, err)
	}
	runtime.deliverShares(ctx, p, available)
	if got := installedBandwidth(t, b, p.ID); got != shareTestFloor(2) {
		t.Fatal("reduction not applied", got)
	}
	// Restart: durable state survives, in-memory acknowledgements do not.
	runtime.mu.Lock()
	runtime.shares = shareRuntime{}
	runtime.mu.Unlock()
	recorder.set(b.node.ID, shareFail)
	for range 4 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal/2 {
		t.Fatal("raised from an acknowledgement the restarted Master cannot know", got)
	}
	recorder.set(b.node.ID, sharePass)
	p = shareStep(t, ctx, runtime, p.ID, available)
	if got := installedBandwidth(t, a, p.ID); got != shareTestTotal-shareTestFloor(2) {
		t.Fatal("not raised after the re-acknowledgement", got)
	}
}

// Idle detection is debounced by steps, not time: a node must be idle in two
// steps in a row. Each flip costs two commits (reduce, then raise).
func TestIdleDebounceCommitCount(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "count-a", recorder), shareTestAgent(t, f, "count-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p = shareStep(t, ctx, runtime, p.ID, available)
	if got := runtime.shareCommits(p.ID); got != 0 {
		t.Fatal("released after a single idle step", got)
	}
	p = shareStep(t, ctx, runtime, p.ID, available)
	if got := runtime.shareCommits(p.ID); got != 2 {
		t.Fatal("expected one reduction and one raise", got)
	}
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := runtime.shareCommits(p.ID); got != 2 {
		t.Fatal("a settled allocation committed again", got)
	}
	// Work for the idle node raises it in the next step, without debounce.
	if _, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 2, Objects: []transferstore.Object{{Key: "more", Size: 1}}}}, map[string]bool{b.node.ID: true}); err != nil {
		t.Fatal(err)
	}
	current, _ := runtime.repo.Get(p.ID)
	if !runtime.releasedNowBusy(current) {
		t.Fatal("work on a released node is not noticed")
	}
	p = shareStep(t, ctx, runtime, p.ID, available)
	if got := runtime.shareCommits(p.ID); got != 4 {
		t.Fatal("expected one reduction and one raise for the busy flip", got)
	}
	if a, b := installedBandwidth(t, a, p.ID), installedBandwidth(t, b, p.ID); a != shareTestTotal/2 || b != shareTestTotal/2 {
		t.Fatal("two busy nodes share equally", a, b)
	}
}

// After a rollback, an older Master sends its equal split at BudgetRevision.
// That revision is above every share revision, so the Agent accepts it; back
// on this release the Agent rejects the share revision as stale, and the
// runtime skips past it instead of treating the node as unavailable.
func TestConflictNeverMarksUnavailable(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "rollback-a", recorder), shareTestAgent(t, f, "rollback-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	old := p
	old.Shares = nil
	for i, agent := range []*takeoverAgent{a, b} {
		if err := agent.service.Limits.ApplyParent(ctx, old.NodePolicy(i)); err != nil {
			t.Fatal("an older Master's policy was rejected", err)
		}
	}
	snapshot, err := storageconfig.NewMaster(f.master.store).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	ready, prepareErr := runtime.prepareMembers(ctx, p, snapshot)
	if prepareErr != nil || !ready[a.node.ID] || !ready[b.node.ID] {
		t.Fatal("a healthy node became unavailable after a rollback", ready, prepareErr)
	}
	current, _ := runtime.repo.Get(p.ID)
	if task.WireRevision(current) <= old.BudgetRevision {
		t.Fatal("the share revision did not move past the older Master's", task.WireRevision(current), old.BudgetRevision)
	}
	for i, agent := range []*takeoverAgent{a, b} {
		policy, err := agent.service.Limits.Parent(p.ID)
		if err != nil || policy != current.NodePolicy(i) {
			t.Fatal("agent does not hold the current share", policy, err)
		}
	}
}

// Lowering the limit reduces at once; no node is raised from the new total
// until every reduction has been acknowledged.
func TestLimitLowerNeverRaisesBeforeAck(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "lower-a", recorder), shareTestAgent(t, f, "lower-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	// B idles at the floor, A holds the rest. Halve the limit while B is
	// unreachable: A is reduced, B keeps its floor and is not raised.
	recorder.set(b.node.ID, shareFail)
	limit := 500.0
	p, err := runtime.repo.UpdateLimits(p.ID, &limit, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	half := shareTestTotal / 2
	var sum int64
	for _, entry := range p.Shares.Nodes {
		sum += entry.Share.Bandwidth
	}
	if sum > half || p.Shares.Nodes[a.node.ID].Share.Bandwidth > half {
		t.Fatal("new shares exceed the lowered total", p.Shares)
	}
	_ = runtime.policies(ctx, p)
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
		reserved := 0.0
		for _, entry := range p.Shares.Nodes {
			reserved += float64(entry.Reserved.Bandwidth)
		}
		if reserved > float64(shareTestTotal) {
			t.Fatal("reserves exceed the larger of the old and new totals", p.Shares)
		}
	}
	// Both Agents together never exceed the old total, and A never exceeds
	// the new one.
	if got := installedBandwidth(t, a, p.ID); got > half {
		t.Fatal("A above the lowered total", got)
	}
}

// A member that stays offline keeps its last share reserved until its lease
// expires; then it is fenced and the busy node receives everything.
func TestOfflineMemberKeepsReserved(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "offline-a", recorder), shareTestAgent(t, f, "offline-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	now := time.Now()
	var clock sync.Mutex
	runtime.leases.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	both := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, both)
	if err != nil || p.Failover == nil || !p.Failover.Enabled {
		t.Fatal("failover not enabled", err)
	}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	recorder.set(b.node.ID, shareFail)
	onlyA := map[string]bool{a.node.ID: true}
	for range 5 {
		p = shareStep(t, ctx, runtime, p.ID, onlyA)
		if got := installedBandwidth(t, a, p.ID); got != shareTestTotal/2 {
			t.Fatal("raised while an offline member may still transfer at its old share", got)
		}
	}
	// The member really goes away (an online one would rejoin at once).
	b.cancel()
	clock.Lock()
	now = now.Add(runtime.leases.ttl + runtime.leases.margin + time.Second)
	clock.Unlock()
	if p, err = runtime.reconcileFailover(ctx, p, map[string]bool{a.node.ID: true}); err != nil {
		t.Fatal(err)
	}
	if !p.NodeRetired(b.node.ID) {
		t.Fatal("offline member not fenced after its lease expired")
	}
	if p.Shares.Nodes[a.node.ID].Share.Bandwidth != shareTestTotal {
		t.Fatal("the remaining node must receive the whole total once the offline one is fenced", p.Shares)
	}
}

// The payload an Agent receives decodes strictly into ratelimit.ParentLimit:
// dynamic shares add no field an older Agent could reject.
func TestBandwidthParentPayloadStrictDecode(t *testing.T) {
	p := task.Parent{ID: 3, Generation: 2, BudgetRevision: 8, Status: "RUNNING", Spec: task.Spec{Nodes: []task.Node{{ID: "a"}, {ID: "b"}}, BandwidthMbps: 1000, RequestLimits: &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 7}}}
	p.Shares = &task.ShareState{Revision: 7, Nodes: map[string]task.NodeShare{
		"a": {Share: task.ShareRates{Bandwidth: 124023438, List: 6.9}, Reserved: task.ShareRates{Bandwidth: 124023438, List: 6.9}},
		"b": {Share: task.ShareRates{Bandwidth: 976562, List: 0.05}, Reserved: task.ShareRates{Bandwidth: 976562, List: 0.05}},
	}}
	for _, parent := range []task.Parent{p, func() task.Parent { q := p; q.Shares = nil; return q }()} {
		for i := range 2 {
			raw, err := json.Marshal(parentPolicy(parent, i))
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var policy ratelimit.ParentLimit
			if err := decoder.Decode(&policy); err != nil {
				t.Fatal("payload has fields an Agent does not know", string(raw), err)
			}
			if policy.Revision == 0 || policy.BandwidthBytesPerSecond <= 0 || math.IsNaN(policy.ListQPS) {
				t.Fatal("invalid payload", string(raw))
			}
		}
	}
	if parentPolicy(p, 0).Revision != 7 || parentPolicy(func() task.Parent { q := p; q.Shares = nil; return q }(), 0).Revision != 8 {
		t.Fatal("wire revisions")
	}
}

// A node joins while another member is offline. The offline member still
// reserves its (small) idle share; the gate grants the joiner's lease as soon
// as the online members' reduced shares and all reserves fit the total,
// without waiting for the offline member.
func TestJoinProceedsWithSlackDespiteOfflinePeer(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b, c := shareTestAgent(t, f, "slack-a", recorder), shareTestAgent(t, f, "slack-b", recorder), shareTestAgent(t, f, "slack-c", recorder)
	waitOnline(t, f, a.node, b.node, c.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	both := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, both)
	if err != nil || !p.Failover.Enabled {
		t.Fatal("failover not enabled", err)
	}
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, both)
	}
	if got := installedBandwidth(t, b, p.ID); got != shareTestFloor(2) {
		t.Fatal("idle member not at its floor", got)
	}
	recorder.set(b.node.ID, shareFail) // b is unreachable from here on
	granted := false
	call := runtime.leases.call
	runtime.leases.call = func(ctx context.Context, session agenttransport.Session, op string, mutating bool, in, out any) error {
		if op == "lease_grant" && session.NodeID == c.node.ID {
			// What each node may enforce: a and c their installed share, b its last share.
			sum := installedBandwidth(t, a, p.ID) + installedBandwidth(t, b, p.ID) + installedBandwidth(t, c, p.ID)
			if sum > shareTestTotal {
				t.Error("joiner granted while the members could exceed the total", sum)
			}
			granted = true
		}
		return call(ctx, session, op, mutating, in, out)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"requestId": "slack-add-c", "add": []string{c.node.ID}}); status != 200 {
		t.Fatal(status, body)
	}
	p, _ = runtime.repo.Get(p.ID)
	if p, err = runtime.reconcileFailover(ctx, p, map[string]bool{a.node.ID: true, c.node.ID: true}); err != nil {
		t.Fatal(err)
	}
	if p.NodeJoining(c.node.ID) || len(p.ActiveNodes()) != 3 {
		t.Fatal("node did not join", p.Membership)
	}
	if err := runtime.leases.renew(ctx, p, c.node); err != nil {
		t.Fatal("joiner blocked by an offline member although the reserves fit", err)
	}
	if !granted {
		t.Fatal("joiner not granted")
	}
	if runtime.shareAcked(p, b.node.ID) {
		t.Fatal("the offline member cannot have acknowledged")
	}
}

// The bandwidth status follows a reallocation: a node that has not applied
// the new revision yet is pending, then applied; the response names the
// allocation mode and each node's demand.
func TestBandwidthStatusFollowsReallocation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	recorder := &shareRecorder{}
	a, b := shareTestAgent(t, f, "status-a", recorder), shareTestAgent(t, f, "status-b", recorder)
	waitOnline(t, f, a.node, b.node)
	p := shareTask(t, f, a, b)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	data := taskLimitsStatusGET(t, f, p.ID)
	if data["allocation"] != "dynamic" || data["applied"] != true {
		t.Fatal("dynamic allocation not applied", data)
	}
	// Commit a reduction for the idle node without delivering it.
	p, changed, err := runtime.repo.ReallocateShares(p.ID, p.Generation, task.WireRevision(p), runtime.shareAckedNodes(p), map[string]bool{b.node.ID: true})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	data = taskLimitsStatusGET(t, f, p.ID)
	if data["applied"] != false || data["pending"] != true {
		t.Fatal("an undelivered reallocation must read as pending", data)
	}
	for _, raw := range data["nodes"].([]any) {
		node := raw.(map[string]any)
		want := "busy"
		if node["address"] == b.node.Address {
			want = "idle"
			if node["adjusting"] != true {
				t.Fatal("an unacknowledged reduction must read as adjusting", node)
			}
		}
		if node["demand"] != want {
			t.Fatal("demand", node)
		}
	}
	runtime.deliverShares(ctx, p, map[string]bool{a.node.ID: true, b.node.ID: true})
	data = taskLimitsStatusGET(t, f, p.ID)
	if data["applied"] != true || data["pending"] != false {
		t.Fatal("delivered reallocation not applied", data)
	}
}
