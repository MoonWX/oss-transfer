package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// oldCapabilityExecutor models an Agent release without recovery fencing.
type oldCapabilityExecutor struct{ *agentservice.Service }

func (a oldCapabilityExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		raw, _ := json.Marshal(map[string]any{"leaseProtocol": 1})
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	}
	return a.Service.Execute(ctx, c)
}

func oldTestAgent(t *testing.T, f *agentHTTPFixture, name string) task.Node {
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
	done := make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, f, credential), oldCapabilityExecutor{service}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; service.Close(context.Background()); db.Close() })
	return task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}
}

func waitOnline(t *testing.T, f *agentHTTPFixture, nodes ...task.Node) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		members, err := f.master.onlineAgents()
		if err != nil {
			t.Fatal(err)
		}
		online := map[string]bool{}
		for _, member := range members {
			online[member.ID] = true
		}
		missing := false
		for _, node := range nodes {
			missing = missing || !online[node.ID]
		}
		if !missing {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Agents did not connect")
}

func postNodes(t *testing.T, f *agentHTTPFixture, id int64, body map[string]any) (int, map[string]any) {
	t.Helper()
	return request(t, f.http.Client(), "POST", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, id), f.admin, body)
}

func appliedBandwidth(t *testing.T, a *takeoverAgent, id int64) int64 {
	t.Helper()
	policy, err := a.service.Limits.Parent(id)
	if err != nil {
		return 0
	}
	return policy.BandwidthBytesPerSecond
}

// Adding a node to a running single-node task: the API records a join, the
// runtime enables fencing, admits the node under a fresh epoch, and grants it
// a lease only after the existing member acknowledged its reduced share.
func TestAddNodeGrantsLeaseOnlyAfterExistingSharesShrink(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "member-a"), takeoverTestAgent(t, f, "member-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node) // 8 Mbps = 1,000,000 B/s
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	total := int64(1_000_000)
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := appliedBandwidth(t, a, p.ID); got != total {
		t.Fatal("initial share", got)
	}
	var mu sync.Mutex
	violations := []string{}
	granted := false
	call := runtime.leases.call
	runtime.leases.call = func(ctx context.Context, session agenttransport.Session, op string, mutating bool, in, out any) error {
		if op == "lease_grant" && session.NodeID == b.node.ID {
			sum := appliedBandwidth(t, a, p.ID) + total/2
			mu.Lock()
			granted = true
			if sum > total {
				violations = append(violations, fmt.Sprint("granted b while a still had ", appliedBandwidth(t, a, p.ID)))
			}
			mu.Unlock()
		}
		return call(ctx, session, op, mutating, in, out)
	}
	status, body := postNodes(t, f, p.ID, map[string]any{"requestId": "add-b", "add": []string{b.node.ID}})
	if status != 200 {
		t.Fatal(status, body)
	}
	replayStatus, replay := postNodes(t, f, p.ID, map[string]any{"requestId": "add-b", "add": []string{b.node.ID}})
	if replayStatus != 200 || replay["data"].(map[string]any)["replayed"] != true {
		t.Fatal("replay not idempotent", replayStatus, replay)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"requestId": "add-b", "remove": []string{a.node.ID}}); status != 409 || body["code"] != "CONFLICT" {
		t.Fatal("request id reuse accepted", status, body)
	}
	p, _ = runtime.repo.Get(p.ID)
	if !p.NodeJoining(b.node.ID) {
		t.Fatal("join not recorded", p.Membership)
	}
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, available)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeJoining(b.node.ID) || len(p.ActiveNodes()) != 2 {
		t.Fatal("node did not join", p.Membership, p.Failover)
	}
	if err := runtime.leases.renew(ctx, p, b.node); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !granted || len(violations) > 0 {
		t.Fatal("budget ordering violated", granted, violations)
	}
	if got := appliedBandwidth(t, a, p.ID); got != total/2 {
		t.Fatal("existing share not reduced", got)
	}
	if got := appliedBandwidth(t, b, p.ID); got != total/2 {
		t.Fatal("new share", got)
	}
	child, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 1, Objects: []transferstore.Object{{Key: "k", Size: 1}}}, {Number: 2, Objects: []transferstore.Object{{Key: "l", Size: 1}}}}, available)
	if err != nil {
		t.Fatal(err)
	}
	placed := map[string]bool{}
	for _, c := range child {
		placed[c.Node.ID] = true
	}
	if !placed[b.node.ID] {
		t.Fatal("added node received no new work", child)
	}
}

// Drain removal fences the node before its share is redistributed; the
// removed node can no longer execute the task's work.
func TestDrainRemovalFencesBeforeRaisingShares(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "drain-a"), takeoverTestAgent(t, f, "drain-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, available)
	if err != nil || !p.Failover.Enabled {
		t.Fatal(err)
	}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	child, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 1, Objects: []transferstore.Object{{Key: "busy", Size: 1}}}}, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	status, body := postNodes(t, f, p.ID, map[string]any{"requestId": "drain-b", "remove": []string{b.node.ID}})
	if status != 200 {
		t.Fatal(status, body)
	}
	p, _ = runtime.repo.Get(p.ID)
	// A running child keeps the node in the task.
	running := execution.Record{Job: child[0].Job, Status: execution.Running, Attempt: 1, StartedAt: new(time.Now()), UpdatedAt: time.Now()}
	if err := runtime.repo.Observe(p.ID, p.Generation, 1, running); err != nil {
		t.Fatal(err)
	}
	if p, err = runtime.reconcileMembership(ctx, p, available); err != nil || p.NodeRemoved(b.node.ID) {
		t.Fatal("drained node left with a running child", err)
	}
	// While it runs a child the draining node is busy and keeps its share.
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	if got := appliedBandwidth(t, b, p.ID); got != 500_000 {
		t.Fatal("a draining node with a running child lost its share", got)
	}
	done := running
	done.Status, done.Attempt, done.UpdatedAt = execution.Completed, 2, time.Now()
	if err := runtime.repo.Observe(p.ID, p.Generation, 1, done); err != nil {
		t.Fatal(err)
	}
	// With no child left it is idle: it drops to the floor first, and only
	// after that reduction is acknowledged is the other node raised.
	for range 3 {
		p = shareStep(t, ctx, runtime, p.ID, available)
	}
	floor := int64(1_000_000 / 128)
	if got := appliedBandwidth(t, b, p.ID); got != floor {
		t.Fatal("drained node kept more than the idle floor", got)
	}
	if got := appliedBandwidth(t, a, p.ID); got != 1_000_000-floor {
		t.Fatal("remaining node not raised by the released share", got)
	}
	fenced := false
	call := runtime.leases.call
	runtime.leases.call = func(ctx context.Context, session agenttransport.Session, op string, mutating bool, in, out any) error {
		if op == "lease_retire" && session.NodeID == b.node.ID {
			if got := appliedBandwidth(t, a, p.ID) + appliedBandwidth(t, b, p.ID); got > 1_000_000 {
				t.Error("shares exceed the total before the leaving node was fenced", got)
			}
			if got := appliedBandwidth(t, a, p.ID); got != 1_000_000-floor {
				t.Error("share raised before the leaving node was fenced", got)
			}
			fenced = true
		}
		return call(ctx, session, op, mutating, in, out)
	}
	p, _ = runtime.repo.Get(p.ID)
	if p, err = runtime.reconcileMembership(ctx, p, available); err != nil {
		t.Fatal(err)
	}
	if !fenced || !p.NodeRemoved(b.node.ID) || p.MembershipPending() || len(p.ActiveNodes()) != 1 {
		t.Fatal("drained node did not leave", fenced, p.Membership)
	}
	if _, err := b.service.Leases.ParentContext(p.ID, p.Generation); !errors.Is(err, agentservice.ErrLeaseRetired) {
		t.Fatal("removed node still authorized", err)
	}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := appliedBandwidth(t, a, p.ID); got != 1_000_000 {
		t.Fatal("remaining share not raised", got)
	}
	// Re-running reconciliation with the removed node online never rejoins it.
	if p, err = runtime.rejoinAvailableNodes(ctx, p); err != nil || !p.NodeRemoved(b.node.ID) {
		t.Fatal("removed node rejoined", err)
	}
}

// Immediate removal revokes running work on a live node, collects its last
// durable progress and continues it elsewhere under a new assignment.
func TestRemoveNowRevokesRunningWorkOnLiveNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "now-a"), takeoverTestAgent(t, f, "now-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, available)
	if err != nil || !p.Failover.Enabled {
		t.Fatal(err)
	}
	rows, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 1, Objects: []transferstore.Object{{Key: "x", Size: 1}, {Key: "y", Size: 1}}}}, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	old := rows[0]
	if _, err := b.service.Jobs.Accept(ctx, old.Job); err != nil {
		t.Fatal(err)
	}
	status, body := postNodes(t, f, p.ID, map[string]any{"requestId": "now-b", "remove": []string{b.node.ID}, "mode": "now"})
	if status != 200 {
		t.Fatal(status, body)
	}
	p, _ = runtime.repo.Get(p.ID)
	if p, err = runtime.reconcileMembership(ctx, p, available); err != nil {
		t.Fatal(err)
	}
	if !p.NodeRemoved(b.node.ID) {
		t.Fatal("node not removed", p.Membership)
	}
	moved, err := runtime.repo.Children(p.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if moved[0].Node.ID != a.node.ID || moved[0].Job.Assignment != old.Job.Assignment+1 {
		t.Fatal("work did not continue on the remaining node", moved[0].Node, moved[0].Job.Assignment)
	}
	if _, err := b.service.Leases.JobContext(old.Job); err == nil {
		t.Fatal("removed node can still execute its old assignment")
	}
	late := execution.Record{Job: old.Job, Status: execution.Completed, Attempt: 9, UpdatedAt: time.Now()}
	late.Job.Generation = p.Generation
	if err := runtime.repo.Observe(p.ID, p.Generation, 1, late); err == nil {
		t.Fatal("removed node's result was committed")
	}
}

func TestNodeChangeAPIRejections(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "reject-a"), takeoverTestAgent(t, f, "reject-b")
	old := oldTestAgent(t, f, "reject-old")
	waitOnline(t, f, a.node, b.node, old)
	p := takeoverParent(t, f, a.node, b.node)
	cases := []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"add": []string{old.ID}}, "AGENT_TOO_OLD"},
		{map[string]any{"remove": []string{a.node.ID, b.node.ID}}, "LAST_NODE"},
		{map[string]any{"remove": []string{"nobody.example"}}, "NOT_MEMBER"},
		{map[string]any{"add": []string{"offline.example"}}, "AGENT_OFFLINE"},
		{map[string]any{"add": []string{a.node.ID}}, "ALREADY_MEMBER"},
		{map[string]any{"remove": []string{a.node.ID}, "expectedNodeRevision": 5}, "STALE"},
	}
	for _, tc := range cases {
		status, body := postNodes(t, f, p.ID, tc.body)
		if status != 409 || body["code"] != tc.code {
			t.Fatal(tc.code, status, body)
		}
	}
	if status, _ := postNodes(t, f, p.ID, map[string]any{"remove": []string{a.node.ID}, "mode": "later"}); status != 400 {
		t.Fatal("invalid mode accepted", status)
	}
	if status, _ := postNodes(t, f, p.ID, map[string]any{"add": []string{}}); status != 400 {
		t.Fatal("empty change accepted", status)
	}
	status, view := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, p.ID), f.admin, nil)
	if status != 200 {
		t.Fatal(status, view)
	}
	data := view["data"].(map[string]any)
	if data["editable"] != true || len(data["nodes"].([]any)) != 2 {
		t.Fatal(data)
	}
	candidates := data["candidates"].([]any)
	if len(candidates) != 1 || candidates[0].(map[string]any)["eligible"] != false {
		t.Fatal("old Agent offered as eligible", candidates)
	}
	// A drain makes the change pending; an unrelated add must wait.
	if status, body := postNodes(t, f, p.ID, map[string]any{"remove": []string{b.node.ID}}); status != 200 {
		t.Fatal(status, body)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"remove": []string{a.node.ID}}); status != 409 || body["code"] != "CHANGE_PENDING" {
		t.Fatal(status, body)
	}
	row, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil)
	if row != 200 {
		t.Fatal(row)
	}
}

func TestNodeChangeRejectsCompletedAndSingleObjectTasks(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "done-a"), takeoverTestAgent(t, f, "done-b")
	waitOnline(t, f, a.node, b.node)
	repo := f.master.tasks.repo
	single, _, err := repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{a.node}, ObjectKey: "one", ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}, "single")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := postNodes(t, f, single.ID, map[string]any{"add": []string{b.node.ID}}); status != 409 || body["code"] != "SINGLE_OBJECT" {
		t.Fatal(status, body)
	}
	done := takeoverParent(t, f, a.node)
	if err := repo.ScanProgress(done.ID, done.Generation, 0, 0, 0, true, ""); err != nil {
		t.Fatal(err)
	}
	if finished, err := repo.FinishIfReady(done.ID, done.Generation); err != nil || !finished {
		t.Fatal(finished, err)
	}
	if status, body := postNodes(t, f, done.ID, map[string]any{"add": []string{b.node.ID}}); status != 409 || body["code"] != "COMPLETED" {
		t.Fatal(status, body)
	}
}
