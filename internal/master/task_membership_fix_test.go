package master

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func onlineAddress(t *testing.T, f *agentHTTPFixture, id string) string {
	t.Helper()
	members, err := f.master.onlineAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.ID == id {
			return member.Address
		}
	}
	t.Fatal("Agent is not online", id)
	return ""
}

// A stopped task without lease fencing cannot accept changes while a current
// member is offline: that member could still hold unfenced work.
func TestStoppedTaskWithoutFencingRejectsChangesWithOfflineMember(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, c := takeoverTestAgent(t, f, "stopped-a"), takeoverTestAgent(t, f, "stopped-c")
	waitOnline(t, f, a.node, c.node)
	ghost := task.Node{ID: "ghost-node", EnrollmentID: "ghost-enrollment", Address: "ghost.example"}
	p := takeoverParent(t, f, a.node, ghost)
	repo := f.master.tasks.repo
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{
		{"add": []string{c.node.ID}},
		{"remove": []string{ghost.ID}, "mode": "now"},
	} {
		if status, response := postNodes(t, f, p.ID, body); status != 409 || response["code"] != "AGENT_OFFLINE" {
			t.Fatal(body, status, response)
		}
	}
}

// Cancelling a drain on a local transfer keeps the node's binding: clients may
// omit it or repeat the same path; a different path is rejected.
func TestLocalDrainCancelAcceptsExistingBinding(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "local-a"), takeoverTestAgent(t, f, "local-b")
	waitOnline(t, f, a.node, b.node)
	root := t.TempDir()
	na, nb := a.node, b.node
	na.LocalRoot, nb.LocalRoot = root, root
	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "LOCAL", Root: root}, Nodes: []task.Node{na, nb}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}, "local-drain")
	if err != nil {
		t.Fatal(err)
	}
	address := onlineAddress(t, f, b.node.ID)
	drain := func(key string) {
		t.Helper()
		if status, body := postNodes(t, f, p.ID, map[string]any{"requestId": key, "remove": []string{b.node.ID}}); status != 200 {
			t.Fatal(status, body)
		}
	}
	drain("drain-1")
	if status, body := postNodes(t, f, p.ID, map[string]any{"add": []string{b.node.ID}, "bindings": []map[string]string{{"executionNodeIp": address, "localPath": "/elsewhere"}}}); status != 400 {
		t.Fatal("changed binding accepted", status, body)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"add": []string{b.node.ID}, "bindings": []map[string]string{{"executionNodeIp": address, "localPath": root}}}); status != 200 {
		t.Fatal("drain cancel with the existing binding rejected", status, body)
	}
	drain("drain-2")
	if status, body := postNodes(t, f, p.ID, map[string]any{"add": []string{b.node.ID}}); status != 200 {
		t.Fatal("drain cancel without a binding rejected", status, body)
	}
	current, _ := f.master.tasks.repo.Get(p.ID)
	if current.MembershipPending() {
		t.Fatal(current.Membership)
	}
}

// A retried request ID replays even as a dry run, so a retry after an unknown
// outcome is not reported as a failed preflight.
func TestDryRunWithSeenRequestIDReplays(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "replay-a"), takeoverTestAgent(t, f, "replay-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node, b.node)
	body := map[string]any{"requestId": "replay-1", "remove": []string{b.node.ID}, "expectedNodeRevision": 0}
	if status, response := postNodes(t, f, p.ID, body); status != 200 {
		t.Fatal(status, response)
	}
	body["dryRun"] = true
	status, response := postNodes(t, f, p.ID, body)
	if status != 200 || response["data"].(map[string]any)["replayed"] != true {
		t.Fatal("dry run of an accepted request did not replay", status, response)
	}
}

// An Agent removed from a running task no longer blocks its decommissioning.
func TestRemovedMemberCanBeDecommissioned(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "decom-a"), takeoverTestAgent(t, f, "decom-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node, b.node)
	address := onlineAddress(t, f, b.node.ID)
	path := fmt.Sprintf("%s/api/master/nodes/%s", f.http.URL, url.PathEscape(address))
	if status, _ := request(t, f.http.Client(), "DELETE", path, f.admin, nil); status != 409 {
		t.Fatal("active member removed from the cluster", status)
	}
	repo := f.master.tasks.repo
	p, err := repo.EnableFailover(p.ID, p.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"remove": []string{b.node.ID}, "mode": "now"}); status != 200 {
		t.Fatal(status, body)
	}
	if _, err := repo.BeginFailover(p.ID, p.Generation, b.node.ID); err != nil {
		t.Fatal(err)
	}
	if status, body := request(t, f.http.Client(), "DELETE", path, f.admin, nil); status != 200 {
		t.Fatal("removed member still blocks decommissioning", status, body)
	}
}

func TestRemovedScannerWithCompleteScanNeedsNoScanner(t *testing.T) {
	p := task.Parent{Status: "RUNNING", ScanComplete: true, DispatchedThrough: 2, ScannedBatches: 2,
		Spec:       task.Spec{Nodes: []task.Node{{ID: "a"}, {ID: "b"}}},
		Membership: &task.MembershipState{Removed: map[string]time.Time{"a": {}}}}
	if !removedScannerDone(p) {
		t.Fatal("removed scanner with a complete scan still required")
	}
	for _, change := range []func(*task.Parent){
		func(p *task.Parent) { p.ScanComplete = false },
		func(p *task.Parent) { p.DispatchedThrough = 1 },
		func(p *task.Parent) { p.Membership = nil },
		func(p *task.Parent) { p.Status = "RESUMING" },
	} {
		copy := p
		change(&copy)
		if removedScannerDone(copy) {
			t.Fatal("scanner wrongly treated as done", copy)
		}
	}
}

func TestLeaseOwnerCurrentParksCancelledJoins(t *testing.T) {
	p := task.Parent{Spec: task.Spec{Nodes: []task.Node{{ID: "a"}, {ID: "b"}}}, Failover: &task.FailoverState{Enabled: true, NodeEpochs: map[string]uint64{"b": 2}, Rejoining: map[string]uint64{"a": 1}}}
	for _, tc := range []struct {
		node  string
		epoch uint64
		want  bool
	}{
		{"b", 2, true},  // current epoch
		{"a", 1, true},  // rejoin in progress
		{"a", 0, true},  // original epoch
		{"b", 1, false}, // stale epoch
		{"c", 1, false}, // cancelled join: no longer a member
	} {
		if got := leaseOwnerCurrent(p, taskLeaseRecord{Node: task.Node{ID: tc.node}, OwnerEpoch: tc.epoch}); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}
