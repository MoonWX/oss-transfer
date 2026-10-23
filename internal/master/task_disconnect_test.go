package master

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type disconnectScanner struct {
	*agentservice.Service
	scanCalls atomic.Int64
}

func (a *disconnectScanner) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	var value any
	switch command.Operation {
	case "scan_batches":
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	case "scan_start":
		a.scanCalls.Add(1)
		var spec discovery.Spec
		if err := json.Unmarshal(command.Payload, &spec); err != nil {
			return agenttransport.Response{}, err
		}
		value = discovery.State{Spec: spec, Generation: spec.Generation, LastBatch: 3, TotalObjects: 3, TotalBytes: 3, ScanComplete: true}
	case "scan_batch":
		var request agentservice.ScanRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return agenttransport.Response{}, err
		}
		value = map[string]any{"found": request.Number < 3, "batch": discovery.Batch{ParentID: request.ParentID, Generation: request.Generation, Number: request.Number + 1, Objects: []transferstore.Object{{Key: fmt.Sprint(request.Number + 1), Size: 1}}}}
	case "scan_ack":
		value = map[string]any{}
	default:
		return a.Service.Execute(ctx, command)
	}
	payload, err := json.Marshal(value)
	return agenttransport.Response{Status: 200, Payload: payload}, err
}

// Use real authenticated outbound transport, distinct durable databases, and
// completed filesystem transfers. Only discovery metadata is scripted: the
// regression concerns coordination after one enrolled peer stops polling.
func TestDisconnectedMemberDoesNotHideHealthyProgressOrReceiveNewWork(t *testing.T) {
	for _, scenario := range []struct{ scannerOffline, alreadyScanned bool }{{false, false}, {true, false}, {true, true}} {
		scannerOffline, alreadyScanned := scenario.scannerOffline, scenario.alreadyScanned
		t.Run(fmt.Sprintf("scanner-offline=%v/scanned=%v", scannerOffline, alreadyScanned), func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			credential := agentCredential(t)
			code, session := f.join(t, f.invite(t, "healthy.example"), "healthy", credential)
			if code != 200 {
				t.Fatal(code)
			}
			healthy := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "healthy.example"}
			code, lostSession := f.join(t, f.invite(t, "lost.example"), "lost", agentCredential(t))
			if code != 200 {
				t.Fatal(code)
			}
			lost := task.Node{ID: lostSession.NodeID, EnrollmentID: lostSession.EnrollmentID, Address: "lost.example"}
			db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			jobs, err := execution.NewRepository(db, execution.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sourceRoot, destinationRoot := t.TempDir(), t.TempDir()
			for _, key := range []string{"1", "2", "3"} {
				if err := os.WriteFile(filepath.Join(sourceRoot, key), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			engine, err := execution.NewEngine(jobs, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
				source, err := transferstore.NewLocal(sourceRoot)
				if err != nil {
					return nil, nil, err
				}
				target, err := transferstore.NewLocal(destinationRoot)
				return source, target, err
			}), execution.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close(context.Background())
			limits, err := ratelimit.New(db, ratelimit.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer limits.Close()
			service := &agentservice.Service{Engine: engine, Jobs: jobs, Configs: storageconfig.NewAgent(db), Limits: limits, Rates: agentservice.NewTaskRates(), Shared: agentservice.NewSharedVerifier()}
			executor := &disconnectScanner{Service: service}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			runner := agenttransport.NewRunner(agentClient(t, f, credential), executor, nil, agenttransport.RunnerOptions{})
			go func() { done <- runner.Run(ctx) }()
			defer func() { cancel(); <-done }()
			nodes := []task.Node{healthy, lost}
			if scannerOffline {
				nodes = []task.Node{lost, healthy}
			}
			spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: nodes, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", BandwidthMbps: 8}
			parent, _, err := f.master.tasks.repo.Create(spec, "disconnect")
			if err != nil {
				t.Fatal(err)
			}
			var assigned []task.Child
			for n := 1; n <= 2; n++ {
				child, err := f.master.tasks.repo.Assign(parent.ID, parent.Generation, n, []transferstore.Object{{Key: fmt.Sprint(n), Size: 1}})
				if err != nil {
					t.Fatal(err)
				}
				assigned = append(assigned, child)
				if child.Node.ID == healthy.ID || alreadyScanned {
					if _, err := engine.Accept(ctx, child.Job); err != nil {
						t.Fatal(err)
					}
					if err := engine.Start(ctx, child.Job.ChildID, parent.Generation); err != nil {
						t.Fatal(err)
					}
					record, err := engine.Wait(ctx, child.Job.ChildID)
					if err != nil || record.Status != execution.Completed {
						t.Fatal("fixture transfer did not complete", record, err)
					}
					if child.Node.ID == lost.ID {
						// This outcome reached Master before the scanner disconnected.
						if err := f.master.tasks.repo.Observe(parent.ID, parent.Generation, child.Job.Number, record); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if alreadyScanned {
				if err := f.master.tasks.repo.ScanProgress(parent.ID, parent.Generation, 2, 2, 2, true, ""); err != nil {
					t.Fatal(err)
				}
			}
			stepCtx, stopStep := context.WithTimeout(ctx, 12*time.Second)
			err = f.master.tasks.step(stepCtx, parent.ID)
			deadlineErr := stepCtx.Err()
			stopStep()
			if (err == nil) != alreadyScanned || deadlineErr != nil {
				t.Fatal("lost member was hidden or consumed the whole coordinator deadline", err)
			}
			current, err := f.master.tasks.repo.Get(parent.ID)
			wantFinished := 1
			if alreadyScanned {
				wantFinished = 2
			}
			if err != nil || current.FinishedChildren != wantFinished || current.Counters.Succeeded != int64(wantFinished) {
				t.Fatal("healthy completion was blocked by disconnected peer", current, err)
			}
			children, err := f.master.tasks.repo.Children(parent.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			for i := range assigned {
				if children[i].Node != assigned[i].Node {
					t.Fatal("existing child was reassigned", children[i])
				}
				if !alreadyScanned && children[i].Node.ID == lost.ID && (children[i].Record != nil || children[i].Accepted) {
					t.Fatal("lost peer's execution result was fabricated")
				}
			}
			if scannerOffline {
				if len(children) != 2 || executor.scanCalls.Load() != 0 {
					t.Fatal("scanner ownership silently moved to another Agent")
				}
			} else {
				if len(children) != 3 || children[2].Node.ID != healthy.ID {
					t.Fatal("new work did not reach the healthy selected node", children)
				}
				if _, err := jobs.Get(ctx, children[2].Job.ChildID); err != nil {
					t.Fatal("healthy Agent did not durably accept new work", err)
				}
			}
			policy, err := limits.Parent(parent.ID)
			if err != nil || policy.BandwidthBytesPerSecond != 500000 {
				t.Fatal("partitioned peer's reserved bandwidth was given away", policy, err)
			}
			wantStatus := "RUNNING"
			if alreadyScanned {
				wantStatus = "COMPLETED"
			}
			if current.Status != wantStatus {
				t.Fatal("temporary disconnection finished or stopped the task", current.Status)
			}
		})
	}
}
