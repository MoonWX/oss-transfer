package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func taskLimitsStatusParent(t *testing.T, f *agentHTTPFixture, nodes ...task.Node) task.Parent {
	t.Helper()
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = nodes
	spec.BandwidthMbps = 100
	spec.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 10, HeadQPS: 20, PutQPS: 30}
	p, _, err := f.master.tasks.repo.Create(spec, "limits-status")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func taskLimitsStatusGET(t *testing.T, f *agentHTTPFixture, id int64) map[string]any {
	t.Helper()
	status, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, id), f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	return body["data"].(map[string]any)
}

func assertTaskLimitsStatus(t *testing.T, data map[string]any, applied, pending bool) {
	t.Helper()
	if data["applied"] != applied || data["pending"] != pending {
		t.Fatalf("expected applied=%v pending=%v, got %v", applied, pending, data)
	}
}

func TestTaskLimitsStatusStopResumeWaitsForAgentPolicy(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	p := taskLimitsStatusParent(t, f, node)
	apply := func() {
		t.Helper()
		if err := service.Limits.ApplyParent(context.Background(), parentPolicy(p, 0)); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(applied, pending bool) {
		t.Helper()
		data := taskLimitsStatusGET(t, f, p.ID)
		assertTaskLimitsStatus(t, data, applied, pending)
		assertTaskLimitsStatus(t, data["nodes"].([]any)[0].(map[string]any), applied, pending)
	}
	apply()
	assert(true, false)
	var err error
	p, _, err = f.master.tasks.repo.RequestStop(p.ID)
	if err != nil || p.Status != "STOPPING" {
		t.Fatal(p, err)
	}
	// The durable intent precedes the asynchronous Agent acknowledgement.
	assert(false, true)
	apply()
	assert(true, false)
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	assert(true, false)
	p, _, err = f.master.tasks.repo.RequestResume(p.ID, nil)
	if err != nil || p.Status != "RESUMING" {
		t.Fatal(p, err)
	}
	assert(false, true)
	apply()
	assert(true, false)
}

func TestTaskLimitsStatusDoesNotHidePolicyFaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		edit   func(*ratelimit.ParentLimit)
	}{
		{"running QPS mismatch", "RUNNING", func(p *ratelimit.ParentLimit) { p.PutQPS++ }},
		{"stopping QPS mismatch", "STOPPING", func(p *ratelimit.ParentLimit) { p.HeadQPS++ }},
		{"resuming QPS mismatch", "RESUMING", func(p *ratelimit.ParentLimit) { p.ListQPS++ }},
		{"stopping future revision", "STOPPING", func(p *ratelimit.ParentLimit) { p.Revision++ }},
		{"resuming future generation", "RESUMING", func(p *ratelimit.ParentLimit) { p.Generation++ }},
		{"stopped stale revision", "STOPPED", func(p *ratelimit.ParentLimit) { p.Revision-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			service, node := taskAPIAgent(t, f)
			p := taskLimitsStatusParent(t, f, node)
			var err error
			if tc.status != "RUNNING" {
				p, _, err = f.master.tasks.repo.RequestStop(p.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.status == "STOPPED" || tc.status == "RESUMING" {
				if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
					t.Fatal(err)
				}
				p, err = f.master.tasks.repo.Get(p.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.status == "RESUMING" {
				p, _, err = f.master.tasks.repo.RequestResume(p.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			policy := parentPolicy(p, 0)
			tc.edit(&policy)
			if err := service.Limits.ApplyParent(context.Background(), policy); err != nil {
				t.Fatal(err)
			}
			data := taskLimitsStatusGET(t, f, p.ID)
			assertTaskLimitsStatus(t, data, false, false)
			assertTaskLimitsStatus(t, data["nodes"].([]any)[0].(map[string]any), false, false)
		})
	}
}

func TestTaskLimitsStatusPendingDoesNotHideOfflineNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	offline := task.Node{ID: "offline", EnrollmentID: "offline-enrollment", Address: "offline.example"}
	p := taskLimitsStatusParent(t, f, node, offline)
	if err := service.Limits.ApplyParent(context.Background(), parentPolicy(p, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.master.tasks.repo.RequestStop(p.ID); err != nil {
		t.Fatal(err)
	}
	data := taskLimitsStatusGET(t, f, p.ID)
	assertTaskLimitsStatus(t, data, false, false)
	nodes := data["nodes"].([]any)
	assertTaskLimitsStatus(t, nodes[0].(map[string]any), false, true)
	assertTaskLimitsStatus(t, nodes[1].(map[string]any), false, false)
	if nodes[1].(map[string]any)["message"] == nil || nodes[1].(map[string]any)["message"] == "" {
		t.Fatal("offline Agent failure lost its explanation", nodes)
	}
}

func TestTaskLimitsStatusMissingAgentPolicyIsNotPending(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	p := taskLimitsStatusParent(t, f, node)
	if _, _, err := f.master.tasks.repo.RequestStop(p.ID); err != nil {
		t.Fatal(err)
	}
	data := taskLimitsStatusGET(t, f, p.ID)
	assertTaskLimitsStatus(t, data, false, false)
	item := data["nodes"].([]any)[0].(map[string]any)
	assertTaskLimitsStatus(t, item, false, false)
	if item["message"] == nil || item["message"] == "" {
		t.Fatal("real Agent policy lookup failure lost its explanation", data)
	}
}

// Delay only the first real policy response, after the Agent has read it. This
// makes a lifecycle change during the Master's RPC deterministic, without sleeps.
type delayedTaskLimitStatus struct {
	*agentservice.Service
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (a *delayedTaskLimitStatus) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	response, err := a.Service.Execute(ctx, c)
	if c.Operation == "bandwidth_parent_status" {
		a.once.Do(func() {
			close(a.entered)
			select {
			case <-a.release:
			case <-ctx.Done():
			}
		})
	}
	return response, err
}

func delayedTaskLimitsAgent(t *testing.T, f *agentHTTPFixture) (*delayedTaskLimitStatus, task.Node) {
	t.Helper()
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, "delayed.example"), "delayed-agent", credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	a := &delayedTaskLimitStatus{Service: service, entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	runner := agenttransport.NewRunner(agentClient(t, f, credential), a, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Agent runner did not stop")
		}
		_ = service.Close(context.Background())
		_ = db.Close()
	})
	// A query is enough to wait for the real reverse connection to become ready;
	// it does not consume the delayed bandwidth_parent_status response.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readyCancel()
	node := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "delayed.example"}
	var nodeStatus map[string]any
	if err := f.master.agentCall(readyCtx, nodeSession(node), "node_status", false, nil, &nodeStatus); err != nil {
		t.Fatal(err)
	}
	return a, node
}

func TestTaskLimitsStatusRechecksLifecycleAfterAgentReply(t *testing.T) {
	for _, missingPolicy := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-policy=%v", missingPolicy), func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			a, node := delayedTaskLimitsAgent(t, f)
			p := taskLimitsStatusParent(t, f, node)
			if !missingPolicy {
				if err := a.Limits.ApplyParent(context.Background(), parentPolicy(p, 0)); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest("GET", fmt.Sprintf("/api/master/tasks/%d/bandwidth-limit", p.ID), nil)
			request.SetPathValue("id", fmt.Sprint(p.ID))
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				f.master.taskBandwidthHTTP(recorder, request)
				close(done)
			}()
			select {
			case <-a.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("policy query did not reach Agent")
			}
			p, _, err := f.master.tasks.repo.RequestStop(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			close(a.release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("policy status request did not finish")
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != 200 {
				t.Fatal(recorder.Code, recorder.Body.String(), err)
			}
			data := body["data"].(map[string]any)
			assertTaskLimitsStatus(t, data, false, !missingPolicy)
			item := data["nodes"].([]any)[0].(map[string]any)
			assertTaskLimitsStatus(t, item, false, !missingPolicy)
			if missingPolicy && (item["message"] == nil || item["message"] == "") {
				t.Fatal("lifecycle race hid Agent query failure", item)
			}
			if err := a.Limits.ApplyParent(context.Background(), parentPolicy(p, 0)); err != nil {
				t.Fatal(err)
			}
			assertTaskLimitsStatus(t, taskLimitsStatusGET(t, f, p.ID), true, false)
		})
	}
}
