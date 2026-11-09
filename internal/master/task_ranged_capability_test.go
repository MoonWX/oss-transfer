package master

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// rangedExecutor is a real Agent that can pose as a release from before the
// ranged-download choice: it advertises no protocol and, like that release's
// strict decoder, rejects a job that carries the field.
type rangedExecutor struct {
	*agentservice.Service
	old     bool
	queries *atomic.Int64
}

func (a rangedExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch {
	case c.Operation == "capabilities":
		a.queries.Add(1)
		caps := a.LeaseCapabilities()
		if a.old {
			delete(caps, "rangedDownloadProtocol")
			delete(caps, "sourceRefreshProtocol")
		}
		raw, _ := json.Marshal(caps)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	case a.old && c.Operation == "child_accept" && bytes.Contains(c.Payload, []byte(`"rangedDownload"`)):
		raw, _ := json.Marshal(map[string]any{"success": false, "message": `json: unknown field "rangedDownload"`})
		return agenttransport.Response{Status: 400, Payload: raw}, nil
	case a.old && bytes.Contains(c.Payload, []byte(`"sourceRefresh"`)):
		raw, _ := json.Marshal(map[string]any{"success": false, "message": `json: unknown field "sourceRefresh"`})
		return agenttransport.Response{Status: 400, Payload: raw}, nil
	}
	return a.Service.Execute(ctx, c)
}

// rangedAgent is one enrolled Agent whose process can be restarted on the
// same database, as an in-place binary upgrade does.
type rangedAgent struct {
	t          *testing.T
	f          *agentHTTPFixture
	node       task.Node
	credential string
	database   string
	service    *agentservice.Service
	queries    atomic.Int64
	stop       func()
}

func rangedTestAgent(t *testing.T, f *agentHTTPFixture, name string, old bool) *rangedAgent {
	t.Helper()
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, name+".example"), name, credential)
	if code != 200 {
		t.Fatal(code)
	}
	a := &rangedAgent{t: t, f: f, credential: credential, database: filepath.Join(t.TempDir(), "agent.db"), node: task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example", LocalRoot: t.TempDir()}}
	a.start(old)
	t.Cleanup(func() { a.stop() })
	return a
}

func (a *rangedAgent) start(old bool) {
	a.t.Helper()
	db, err := state.Open(a.database)
	if err != nil {
		a.t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		a.t.Fatal(err)
	}
	a.service = service
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(a.t, a.f, a.credential), rangedExecutor{Service: service, old: old, queries: &a.queries}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(ctx) }()
	a.stop = func() { cancel(); <-done; service.Close(context.Background()); db.Close() }
}

// upgrade restarts the Agent process on its database as a current release.
func (a *rangedAgent) upgrade() {
	a.stop()
	a.start(false)
}

func rangedDownloadTask(t *testing.T, runtime *taskRuntime, node task.Node, name, choice string) (task.Parent, task.Child) {
	t.Helper()
	spec := task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "LOCAL"}, Nodes: []task.Node{node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", RangedDownload: choice}
	p, _, err := runtime.repo.Create(spec, name)
	if err != nil {
		t.Fatal(err)
	}
	child, err := runtime.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "large.bin", Size: 1 << 30, ETag: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if child.Job.RangedDownload != choice {
		t.Fatal("stored child job changed the choice")
	}
	return p, child
}

// retryAgentCall repeats an idempotent Master-to-Agent call, as the task loop
// does on its next cycle: a call can time out while a restarted Agent
// reconnects. ctx is the hang guard.
func retryAgentCall(t *testing.T, ctx context.Context, call func() error) {
	t.Helper()
	for {
		err := call()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("Agent call kept failing: %v", err)
		}
		time.Sleep(10 * time.Millisecond) // pacing between attempts, not a deadline
	}
}

// observeAndReport runs the Master's observation of one child, then its
// report export after the child has ended, through the real Agent calls.
func observeAndReport(t *testing.T, ctx context.Context, runtime *taskRuntime, agent *rangedAgent, p task.Parent, child task.Child) execution.Record {
	t.Helper()
	var record *execution.Record
	retryAgentCall(t, ctx, func() (err error) {
		record, err = runtime.observeOne(ctx, runtime.newRangedPass(), p, child, nil)
		return err
	})
	if record == nil {
		t.Fatal("observation returned no record")
	}
	if err := runtime.repo.Observe(p.ID, p.Generation, 1, *record); err != nil {
		t.Fatalf("Master rejected the Agent's record: %v", err)
	}
	if err := agent.service.Engine.Stop(ctx, child.Job.ChildID, p.Generation); err != nil {
		t.Fatal(err)
	}
	online := map[string]string{agent.node.ID: agent.node.EnrollmentID}
	if _, _, err := runtime.server.reportChild(ctx, p, child, online, map[string]string{}); err != nil {
		t.Fatalf("report export rejected the child: %v", err)
	}
	return *record
}

// A current Agent receives the task's choice spelled out. An older Agent
// receives no field (which it would reject) and stores none; after an
// in-place upgrade its recovered child still has none, which streams, so an
// "off" choice survives the upgrade. Observation and report export accept
// every combination.
func TestRangedDownloadChoiceAcrossAgentVersions(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // hang guard only
	defer cancel()
	runtime := f.master.tasks
	for _, test := range []struct {
		name, choice, delivered string
		old                     bool
	}{
		{"new-auto", "", execution.RangedDownloadAuto, false},
		{"new-off", execution.RangedDownloadOff, execution.RangedDownloadOff, false},
		{"old-auto", "", "", true},
		{"old-off", execution.RangedDownloadOff, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := rangedTestAgent(t, f, "ranged-"+test.name, test.old)
			p, child := rangedDownloadTask(t, runtime, agent.node, test.name, test.choice)
			for range 2 { // dispatch, then a redelivery
				if err := runtime.deliverChildren(ctx, p, []task.Child{child}); err != nil {
					t.Fatalf("delivery failed: %v", err)
				}
			}
			// Only a current Agent is told that the Master accepts refreshed
			// source pins; an older one would reject the field.
			if fence, err := agent.service.Jobs.Parent(ctx, p.ID); err != nil || fence.SourceRefresh == test.old {
				t.Fatalf("sourceRefresh delivered=%v to an Agent with old=%v (%v)", fence.SourceRefresh, test.old, err)
			}
			if test.old {
				agent.upgrade() // the stored child is recovered by the new binary
			}
			record, err := agent.service.Engine.Get(ctx, child.Job.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Job.RangedDownload != test.delivered {
				t.Fatalf("Agent holds choice %q, want %q", record.Job.RangedDownload, test.delivered)
			}
			observed := observeAndReport(t, ctx, runtime, agent, p, child)
			if observed.Job.RangedDownload != test.delivered {
				t.Fatalf("observation reports %q", observed.Job.RangedDownload)
			}
		})
	}
}

// Only a positive answer is reused: an Agent that does not support the field
// (or cannot be asked) is asked again on every delivery.
func TestRangedSupportRemembersOnlyPositiveAnswers(t *testing.T) {
	var support rangedDownloadSupport
	queries := map[string]int{}
	current := deliveryProtocols{RangedDownloadProtocol: agentservice.RangedDownloadProtocol, SourceRefreshProtocol: agentservice.SourceRefreshProtocol}
	// "previous" is a release with ranged downloads but without source refresh.
	protocols := map[string]deliveryProtocols{"new": current, "previous": {RangedDownloadProtocol: agentservice.RangedDownloadProtocol}, "old": {}}
	query := func(_ context.Context, node task.Node) (deliveryProtocols, error) {
		queries[node.ID]++
		if node.ID == "offline" {
			return deliveryProtocols{}, context.DeadlineExceeded
		}
		return protocols[node.ID], nil
	}
	for range 3 {
		for id, want := range map[string]deliverySupport{"new": {ranged: true, refresh: true}, "previous": {ranged: true}, "old": {}} {
			if got, err := support.supported(context.Background(), task.Node{ID: id}, query); err != nil || got != want {
				t.Fatalf("%s: supported=%+v err=%v", id, got, err)
			}
		}
		if _, err := support.supported(context.Background(), task.Node{ID: "offline"}, query); err == nil {
			t.Fatal("failed query reported an answer")
		}
	}
	if queries["new"] != 1 || queries["previous"] != 3 || queries["old"] != 3 || queries["offline"] != 3 {
		t.Fatalf("queries %v", queries)
	}
	// An Agent upgraded after answering "unsupported" is recognised at once.
	protocols["old"] = current
	if got, err := support.supported(context.Background(), task.Node{ID: "old"}, query); err != nil || !got.ranged || !got.refresh {
		t.Fatal("upgraded Agent still treated as unsupported", err)
	}
}

// An S3 delivery asks for source refresh support only for a child with an
// object whose source changed, and says so only to an Agent that supports it.
func TestDeliveryAllowsSourceRefreshOnlyWhereItMatters(t *testing.T) {
	runtime := &taskRuntime{}
	queries := 0
	protocols := deliveryProtocols{}
	runtime.rangedQuery = func(context.Context, task.Node) (deliveryProtocols, error) {
		queries++
		return protocols, nil
	}
	job := execution.Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}}
	changed := &execution.Record{Job: job, Results: []execution.ItemResult{{ID: "a", Status: execution.ItemFailed, Reconciliation: execution.ReasonSourceChanged}}}
	other := &execution.Record{Job: job, Results: []execution.ItemResult{{ID: "a", Status: execution.ItemFailed}}}
	for _, test := range []struct {
		name    string
		record  *execution.Record
		current bool
		queried bool
		allowed bool
	}{
		{"first delivery", nil, true, false, false},
		{"other failure", other, true, false, false},
		{"changed, current Agent", changed, true, true, true},
		{"changed, previous Agent", changed, false, true, false},
	} {
		queries, protocols = 0, deliveryProtocols{}
		if test.current {
			protocols.SourceRefreshProtocol = agentservice.SourceRefreshProtocol
		}
		runtime.rangedSupport = rangedDownloadSupport{}
		request, err := runtime.acceptRequest(context.Background(), runtime.newRangedPass(), task.Parent{}, task.Child{Job: job, Record: test.record, Node: task.Node{ID: "n"}})
		if err != nil || request.SourceRefresh != test.allowed || (queries > 0) != test.queried {
			t.Fatalf("%s: sourceRefresh=%v queries=%d err=%v", test.name, request.SourceRefresh, queries, err)
		}
	}
}
