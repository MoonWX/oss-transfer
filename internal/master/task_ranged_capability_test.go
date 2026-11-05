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

type rangedAgent struct {
	node    task.Node
	service *agentservice.Service
	queries atomic.Int64
}

func rangedTestAgent(t *testing.T, f *agentHTTPFixture, name string, old bool) *rangedAgent {
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
	a := &rangedAgent{node: task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example", LocalRoot: t.TempDir()}, service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, f, credential), rangedExecutor{Service: service, old: old, queries: &a.queries}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; service.Close(context.Background()); db.Close() })
	return a
}

// An "off" download is delivered to an older Agent without the field, which
// it would reject; that Agent streams anyway. A current Agent receives and
// keeps the choice. Both records are accepted back by the Master, and a later
// delivery to the same child (an upgraded Agent) adds the choice.
func TestRangedDownloadOffReachesOldAndNewAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime := f.master.tasks
	for _, old := range []bool{true, false} {
		name := map[bool]string{true: "ranged-old", false: "ranged-new"}[old]
		agent := rangedTestAgent(t, f, name, old)
		spec := task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "LOCAL"}, Nodes: []task.Node{agent.node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", RangedDownload: execution.RangedDownloadOff}
		p, _, err := runtime.repo.Create(spec, name)
		if err != nil {
			t.Fatal(err)
		}
		child, err := runtime.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "large.bin", Size: 1 << 30, ETag: "v1"}})
		if err != nil {
			t.Fatal(err)
		}
		if child.Job.RangedDownload != execution.RangedDownloadOff {
			t.Fatal("stored child job lost the choice")
		}
		for range 2 { // dispatch, then a redelivery from observation
			if err := runtime.deliverChildren(ctx, p, []task.Child{child}); err != nil {
				t.Fatalf("%s: delivery failed: %v", name, err)
			}
		}
		record, err := agent.service.Engine.Get(ctx, child.Job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{true: "", false: execution.RangedDownloadOff}[old]
		if record.Job.RangedDownload != want {
			t.Fatalf("%s: Agent holds choice %q, want %q", name, record.Job.RangedDownload, want)
		}
		if err := runtime.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
			t.Fatalf("%s: Master rejected the Agent's record: %v", name, err)
		}
		// Old Agents are asked again on every delivery; a positive answer is reused.
		if queries := agent.queries.Load(); old && queries != 2 || !old && queries != 1 {
			t.Fatalf("%s: %d capability queries", name, queries)
		}
		if !old {
			continue
		}
		// The same child delivered again once the Agent supports the choice.
		upgraded := withoutRangedDownload(childAcceptance(p, child))
		upgraded.Job.RangedDownload = execution.RangedDownloadOff
		if _, err := agent.service.Engine.Accept(ctx, upgraded.Job); err != nil {
			t.Fatalf("upgraded Agent rejected the redelivered child: %v", err)
		}
		if record, _ = agent.service.Engine.Get(ctx, child.Job.ChildID); record.Job.RangedDownload != execution.RangedDownloadOff {
			t.Fatal("redelivery did not add the choice")
		}
		if err := runtime.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
			t.Fatalf("Master rejected the upgraded record: %v", err)
		}
	}
}

// A takeover seed carries its own copy of the job; both lose the field.
func TestWithoutRangedDownloadClearsTheSeedToo(t *testing.T) {
	job := execution.Job{ChildID: "1-1", RangedDownload: execution.RangedDownloadOff, Assignment: 2}
	seed := &execution.Record{Job: job}
	request := withoutRangedDownload(agentservice.ChildAcceptRequest{Job: job, Seed: seed})
	if request.Job.RangedDownload != "" || request.Seed.Job.RangedDownload != "" {
		t.Fatal("field left in the request", request)
	}
	if seed.Job.RangedDownload != execution.RangedDownloadOff {
		t.Fatal("the stored record was modified")
	}
}
