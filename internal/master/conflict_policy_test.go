package master

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestConflictPolicyNormalization(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		policy    string
		overwrite *bool
		want      string
		ok        bool
	}{
		{"", nil, "SKIP", true},
		{"", &yes, "OVERWRITE", true},
		{"OVERWRITE_CHANGED", nil, "OVERWRITE_CHANGED", true},
		{"OVERWRITE_CHANGED", &no, "OVERWRITE_CHANGED", true},
		{"OVERWRITE_CHANGED", &yes, "", false},
		{"overwrite_changed", nil, "", false},
		{"CHANGED", nil, "", false},
	} {
		row := ossTaskRequest()
		row.ConflictPolicy, row.Overwrite = tc.policy, tc.overwrite
		spec, _, _, err := normalizeTaskRequest(row, "add-buckets")
		if (err == nil) != tc.ok || (tc.ok && spec.ConflictPolicy != tc.want) {
			t.Errorf("%q overwrite=%v: policy %q err %v", tc.policy, tc.overwrite, spec.ConflictPolicy, err)
		}
	}
}

// preConflictExecutor answers capabilities like a current Agent that
// predates OVERWRITE_CHANGED: it can join running tasks but does not
// advertise conflictPolicyProtocol.
type preConflictExecutor struct{ *agentservice.Service }

func (a preConflictExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		caps := a.LeaseCapabilities()
		delete(caps, "conflictPolicyProtocol")
		raw, _ := json.Marshal(caps)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	}
	return a.Service.Execute(ctx, c)
}

func preConflictTestAgent(t *testing.T, f *agentHTTPFixture, name string) task.Node {
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
	runner := agenttransport.NewRunner(agentClient(t, f, credential), preConflictExecutor{service}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; service.Close(context.Background()); db.Close() })
	return task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}
}

// A task with OVERWRITE_CHANGED is only accepted for, and only joined by,
// Agents that advertise the policy; SKIP and OVERWRITE stay available to
// older Agents.
func TestOverwriteChangedRequiresCapableAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current := takeoverTestAgent(t, f, "conflict-current")
	other := takeoverTestAgent(t, f, "conflict-other")
	older := preConflictTestAgent(t, f, "conflict-older")
	waitOnline(t, f, current.node, other.node, older)
	ctx := context.Background()
	resolve := func(policy string, nodes ...task.Node) error {
		row := ossTaskRequest()
		row.ConflictPolicy = policy
		row.EligibleWorkerNodes = nil
		for _, node := range nodes {
			row.EligibleWorkerNodes = append(row.EligibleWorkerNodes, node.Address)
		}
		_, _, err := f.master.resolveTaskSpec(ctx, row, "add-buckets")
		return err
	}
	if err := resolve(execution.ConflictOverwriteChanged, current.node, other.node); err != nil {
		t.Fatal("current Agents refused:", err)
	}
	err := resolve(execution.ConflictOverwriteChanged, current.node, older)
	if err == nil || !strings.Contains(err.Error(), "does not support overwriting changed objects") {
		t.Fatal("older Agent accepted for OVERWRITE_CHANGED:", err)
	}
	for _, policy := range []string{execution.ConflictSkip, execution.ConflictOverwrite} {
		if err := resolve(policy, current.node, older); err != nil {
			t.Fatalf("%s refused for an older Agent: %v", policy, err)
		}
	}

	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{current.node}, ConflictPolicy: execution.ConflictOverwriteChanged, DistributionMode: "BALANCED", BandwidthMbps: 8}, "conflict-join")
	if err != nil {
		t.Fatal(err)
	}
	status, view := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, p.ID), f.admin, nil)
	if status != 200 {
		t.Fatal(status, view)
	}
	eligible := map[string]any{}
	for _, row := range view["data"].(map[string]any)["candidates"].([]any) {
		candidate := row.(map[string]any)
		eligible[candidate["id"].(string)] = candidate["eligible"]
	}
	if eligible[other.node.ID] != true || eligible[older.ID] != false {
		t.Fatal("candidates ignore the conflict policy:", eligible)
	}
	if status, body := postNodes(t, f, p.ID, map[string]any{"add": []string{older.ID}}); status != 409 || body["code"] != "AGENT_TOO_OLD" {
		t.Fatal("older Agent joined an OVERWRITE_CHANGED task:", status, body)
	}
}
