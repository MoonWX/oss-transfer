package master

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type recoveryAgent struct {
	*agentservice.Service
	old bool
}

func (a recoveryAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		return agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"bootId":"recovery-test-boot"}`)}, nil
	}
	if a.old && c.Operation == "child_recover" {
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
	return a.Service.Execute(ctx, c)
}

// The fixture uses the authenticated HTTP route, outbound transport, real
// execution ledger, rate policies and filesystem probes. Its data-plane factory
// stays blocked so the test can inspect an admitted attempt deterministically.
func recoveryHTTPFixture(t *testing.T, shared, old bool) (*agentHTTPFixture, *agentservice.Service, task.Parent, task.Child, *atomic.Int64) {
	t.Helper()
	f := newAgentHTTPFixture(t)
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, "recovery.example"), "recovery-node", credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := ratelimit.New(db, ratelimit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	node := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "recovery.example", LocalRoot: t.TempDir()}
	spec := task.Spec{Direction: "LOCAL_TO_OSS", Source: execution.Endpoint{Kind: "LOCAL", Root: node.LocalRoot}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", SharedLocalSource: shared, BandwidthMbps: 8, RequestLimits: &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 3, HeadQPS: 5, PutQPS: 7}}
	parent, _, err := f.master.tasks.repo.Create(spec, "recover-http")
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.master.tasks.repo.Assign(parent.ID, parent.Generation, 1, []transferstore.Object{{Key: "file", Size: 1, ETag: "local:fixture", ModifiedAt: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(jobs, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return nil, nil, context.Canceled
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Accept(context.Background(), child.Job); err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(context.Background(), child.Job.ChildID, parent.Generation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	record, err := engine.Wait(ctx, child.Job.ChildID)
	cancel()
	if err != nil || record.Status != execution.Recoverable {
		t.Fatal("fixture did not persist interrupted child", record, err)
	}
	engine.Close(context.Background())
	opens := &atomic.Int64{}
	engine, err = execution.NewEngine(jobs, execution.StoreFactoryFunc(func(ctx context.Context, _ execution.Job) (transferstore.Store, transferstore.Store, error) {
		opens.Add(1)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}), execution.Options{MaxConcurrentJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	service := &agentservice.Service{Engine: engine, Jobs: jobs, Shared: agentservice.NewSharedVerifier(), Limits: limits, Configs: storageconfig.NewAgent(db)}
	runCtx, stop := context.WithCancel(context.Background())
	runner := agenttransport.NewRunner(agentClient(t, f, credential), recoveryAgent{service, old}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		<-done
		engine.Close(context.Background())
		limits.Close()
		db.Close()
	})
	return f, service, parent, child, opens
}

func TestChildRecoveryHTTPUsesCurrentPoliciesAndIsIdempotent(t *testing.T) {
	f, agent, parent, child, _ := recoveryHTTPFixture(t, true, false)
	url := fmt.Sprintf("%s/api/master/tasks/%d/children/1/recover", f.http.URL, parent.ID)
	if code, _ := request(t, f.http.Client(), "POST", url, "", nil); code != 401 {
		t.Fatal("unauthenticated recovery allowed", code)
	}
	code, body := request(t, f.http.Client(), "POST", url, f.admin, nil)
	if code != 200 || body["data"].(map[string]any)["outcome"] != "STARTED" {
		t.Fatal("recovery failed", code, body)
	}
	first, err := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	if err != nil || first.Status != execution.Running || first.Counters.Succeeded != 0 {
		t.Fatal("recovery did not admit existing job", first, err)
	}
	responseChild := body["data"].(map[string]any)["child"].(map[string]any)
	if responseChild["attempt"] != float64(first.Attempt) || responseChild["observedAt"] != float64(first.UpdatedAt.UnixMilli()) {
		t.Fatal("recovery response does not identify the admitted attempt", responseChild, first)
	}
	policy, err := agent.Limits.Parent(parent.ID)
	if err != nil || policy.BandwidthBytesPerSecond != 1000000 || policy.ListQPS != 3 || policy.HeadQPS != 5 || policy.PutQPS != 7 {
		t.Fatal("manual recovery bypassed latest policy", policy, err)
	}
	if !agent.Shared.Allows(child.Job) {
		t.Fatal("manual recovery bypassed shared directory proof")
	}
	entries, err := os.ReadDir(parent.Spec.Nodes[0].LocalRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("manual recovery retained probe", entries, err)
	}
	code, body = request(t, f.http.Client(), "POST", url, f.admin, nil)
	if code != 200 || body["data"].(map[string]any)["outcome"] != "ALREADY_RUNNING" {
		t.Fatal("duplicate recovery did not report running", code, body)
	}
	second, _ := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	current, _ := f.master.tasks.repo.Get(parent.ID)
	if second.Attempt != first.Attempt || current.ChildCount != 1 || current.Generation != parent.Generation {
		t.Fatal("duplicate recovery changed child identity or attempt", second, current)
	}
}

func TestChildRecoveryHTTPRejectsStoppedMissingMountAndOldAgent(t *testing.T) {
	for _, mode := range []string{"stopped", "missing-mount", "old-agent", "missing-child"} {
		t.Run(mode, func(t *testing.T) {
			f, agent, parent, child, opens := recoveryHTTPFixture(t, mode == "missing-mount", mode == "old-agent")
			if mode == "stopped" {
				if _, err := f.master.tasks.repo.Stop(parent.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-mount" {
				if err := f.master.tasks.ensureSharedSource(context.Background(), parent); err != nil {
					t.Fatal("initial shared proof failed", err)
				}
				if err := os.Remove(parent.Spec.Nodes[0].LocalRoot); err != nil {
					t.Fatal(err)
				}
			}
			number := 1
			if mode == "missing-child" {
				number = 2
			}
			url := fmt.Sprintf("%s/api/master/tasks/%d/children/%d/recover", f.http.URL, parent.ID, number)
			code, body := request(t, f.http.Client(), "POST", url, f.admin, nil)
			if code != 409 && !(mode == "missing-child" && code == 404) {
				t.Fatal("unsafe recovery accepted", mode, code, body)
			}
			if mode == "old-agent" && !strings.Contains(fmt.Sprint(body), "Upgrade this Agent") {
				t.Fatal("old Agent error not actionable", body)
			}
			record, err := agent.Jobs.Get(context.Background(), child.Job.ChildID)
			if err != nil || record.Status != execution.Recoverable || opens.Load() != 0 {
				t.Fatal("rejected recovery admitted I/O", record, err, opens.Load())
			}
		})
	}
}

func TestChildRecoveryHTTPReportsCapacityWithoutStartingAnotherAttempt(t *testing.T) {
	f, agent, parent, child, _ := recoveryHTTPFixture(t, false, false)
	blocker := child.Job
	blocker.Number, blocker.ChildID = 2, fmt.Sprintf("%d-2", parent.ID)
	if _, err := agent.Engine.Accept(context.Background(), blocker); err != nil {
		t.Fatal(err)
	}
	if err := agent.Engine.Start(context.Background(), blocker.ChildID, parent.Generation); err != nil {
		t.Fatal(err)
	}
	before, _ := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	url := fmt.Sprintf("%s/api/master/tasks/%d/children/1/recover", f.http.URL, parent.ID)
	start := time.Now()
	code, body := request(t, f.http.Client(), "POST", url, f.admin, nil)
	if code != 200 || body["data"].(map[string]any)["outcome"] != "WAITING_CAPACITY" || time.Since(start) > 3*time.Second {
		t.Fatal("full Agent pool was not reported promptly", code, body)
	}
	after, _ := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	if after.Status != execution.Recoverable || after.Attempt != before.Attempt || after.Counters != before.Counters {
		t.Fatal("capacity response modified interrupted job", before, after)
	}
	active, limit := agent.Engine.Concurrency()
	if active != 1 || limit != 1 {
		t.Fatal("manual recovery exceeded concurrency", active, limit)
	}
}

func TestChildRecoveryHTTPConcurrentClicksAdmitOnlyOneAttempt(t *testing.T) {
	f, agent, parent, child, opens := recoveryHTTPFixture(t, false, false)
	before, err := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/api/master/tasks/%d/children/1/recover", f.http.URL, parent.ID)
	type response struct {
		code int
		body map[string]any
	}
	results := make(chan response, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, body := request(t, f.http.Client(), "POST", url, f.admin, nil)
			results <- response{code, body}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	started, running := 0, 0
	for result := range results {
		if result.code != 200 {
			t.Fatal(result.code, result.body)
		}
		switch result.body["data"].(map[string]any)["outcome"] {
		case "STARTED":
			started++
		case "ALREADY_RUNNING":
			running++
		default:
			t.Fatal(result.body)
		}
	}
	if started != 1 || running != 7 {
		t.Fatal(started, running)
	}
	after, err := agent.Jobs.Get(context.Background(), child.Job.ChildID)
	if err != nil || after.Attempt != before.Attempt+1 || after.Job.Generation != before.Job.Generation {
		t.Fatal(after, err)
	}
	deadline := time.Now().Add(time.Second)
	for opens.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if opens.Load() != 1 {
		t.Fatal("duplicate data-plane execution", opens.Load())
	}
}
