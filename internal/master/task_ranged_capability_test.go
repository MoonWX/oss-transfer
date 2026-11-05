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
		}
		raw, _ := json.Marshal(caps)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	case a.old && c.Operation == "child_accept" && bytes.Contains(c.Payload, []byte(`"rangedDownload"`)):
		raw, _ := json.Marshal(map[string]any{"success": false, "message": `json: unknown field "rangedDownload"`})
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
	protocols := map[string]int{"new": agentservice.RangedDownloadProtocol, "old": 0}
	query := func(_ context.Context, node task.Node) (int, error) {
		queries[node.ID]++
		if node.ID == "offline" {
			return 0, context.DeadlineExceeded
		}
		return protocols[node.ID], nil
	}
	for range 3 {
		for id, want := range map[string]bool{"new": true, "old": false} {
			if got, err := support.supported(context.Background(), task.Node{ID: id}, query); err != nil || got != want {
				t.Fatalf("%s: supported=%v err=%v", id, got, err)
			}
		}
		if _, err := support.supported(context.Background(), task.Node{ID: "offline"}, query); err == nil {
			t.Fatal("failed query reported an answer")
		}
	}
	if queries["new"] != 1 || queries["old"] != 3 || queries["offline"] != 3 {
		t.Fatalf("queries %v", queries)
	}
	// An Agent upgraded after answering "unsupported" is recognised at once.
	protocols["old"] = agentservice.RangedDownloadProtocol
	if got, err := support.supported(context.Background(), task.Node{ID: "old"}, query); err != nil || !got {
		t.Fatal("upgraded Agent still treated as unsupported", err)
	}
}

// An Agent asked while it was old and then upgraded in place receives the
// task's choice on its next delivery, for Auto and Off tasks alike.
func TestUpgradedAgentReceivesTheChoiceOnItsNextDelivery(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // hang guard only
	defer cancel()
	runtime := f.master.tasks
	for _, test := range []struct{ name, choice, delivered string }{
		{"auto", "", execution.RangedDownloadAuto},
		{"off", execution.RangedDownloadOff, execution.RangedDownloadOff},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := rangedTestAgent(t, f, "ranged-upgrade-"+test.name, true)
			p, child := rangedDownloadTask(t, runtime, agent.node, "upgrade-"+test.name, test.choice)
			if supported, err := runtime.rangedSupport.supported(ctx, agent.node, runtime.rangedDownloadProtocol); err != nil || supported {
				t.Fatal("old Agent reported support", err)
			}
			agent.upgrade()
			retryAgentCall(t, ctx, func() error { return runtime.deliverChildren(ctx, p, []task.Child{child}) })
			record, err := agent.service.Engine.Get(ctx, child.Job.ChildID)
			if err != nil || record.Job.RangedDownload != test.delivered {
				t.Fatalf("upgraded Agent received %q, want %q (%v)", record.Job.RangedDownload, test.delivered, err)
			}
			observeAndReport(t, ctx, runtime, agent, p, child)
		})
	}
}

// A takeover seed carries its own copy of the job; both get the same choice.
func TestRangedDownloadChoiceAppliesToTheSeed(t *testing.T) {
	job := execution.Job{ChildID: "1-1", RangedDownload: execution.RangedDownloadOff, Assignment: 2}
	seed := &execution.Record{Job: job}
	for _, choice := range []string{"", execution.RangedDownloadAuto} {
		request := withRangedDownload(agentservice.ChildAcceptRequest{Job: job, Seed: seed}, choice)
		if request.Job.RangedDownload != choice || request.Seed.Job.RangedDownload != choice {
			t.Fatal("job and seed disagree", request)
		}
	}
	if seed.Job.RangedDownload != execution.RangedDownloadOff {
		t.Fatal("the stored record was modified")
	}
	if agentservice.ReportJobDigest(job) != agentservice.ReportJobDigest(execution.Job{ChildID: "1-1", Assignment: 2}) {
		t.Fatal("report digest depends on the ranged-download choice")
	}
}
