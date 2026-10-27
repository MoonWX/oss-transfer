package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
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

func progressMasterFixture(t *testing.T, nodes int) (*agentHTTPFixture, task.Parent) {
	t.Helper()
	f := newAgentHTTPFixture(t)
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "target"}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	for i := range nodes {
		node := task.Node{ID: fmt.Sprintf("node-%d", i), EnrollmentID: "enrollment", Address: fmt.Sprintf("10.0.0.%d", i+1)}
		spec.Nodes = append(spec.Nodes, node)
		if err := f.master.agents.broker.Activate(nodeSession(node)); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := f.master.tasks.repo.Create(spec, "live-progress")
	if err != nil {
		t.Fatal(err)
	}
	// Start the fixture one minute ago so averages can be checked without sleeps.
	started := time.Now().Add(-time.Minute)
	p.StartedAt, p.ActiveTime = &started, &execution.ActiveTime{StartedAt: &started}
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), p)
	}); err != nil {
		t.Fatal(err)
	}
	return f, p
}

func assignProgressChild(t *testing.T, f *agentHTTPFixture, p task.Parent, number int, objects []transferstore.Object) task.Child {
	t.Helper()
	c, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, objects)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, object := range objects {
		size += object.Size
	}
	record := execution.Record{Job: c.Job, Attempt: 1, Status: execution.Running, UpdatedAt: time.Now(), StartedAt: p.StartedAt,
		ActiveTime: &execution.ActiveTime{StartedAt: p.StartedAt}, Counters: execution.Counters{Total: int64(len(objects)), TotalBytes: size}}
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, number, record); err != nil {
		t.Fatal(err)
	}
	c.Record = &record
	return c
}

func childProgressSample(c task.Child, active int, transferred int64) execution.ChildProgress {
	return execution.ChildProgress{ChildID: c.Job.ChildID, Number: c.Job.Number, Generation: c.Job.Generation, Assignment: c.Job.Assignment,
		Attempt: 1, ActiveObjects: active, InFlightBytes: transferred}
}

// Exercise command correlation, enrollment ownership and JSON decoding through
// the same broker used by real Agent RPCs, without starting a transfer worker.
func serveProgressAgent(t *testing.T, f *agentHTTPFixture, node task.Node, respond func(agenttransport.Command) (any, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for {
			command, err := f.master.agents.broker.Poll(ctx, nodeSession(node))
			if err != nil {
				done <- err
				return
			}
			if err = f.master.agents.broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
				done <- err
				return
			}
			value, err := respond(command)
			if err != nil {
				done <- err
				return
			}
			payload, err := json.Marshal(value)
			if err == nil {
				err = f.master.agents.broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}})
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
}

func progressMetrics(p task.Parent, reports ...execution.ChildProgress) agentservice.TaskRateSnapshot {
	return agentservice.TaskRateSnapshot{ParentID: p.ID, Generation: p.Generation, ProgressSupported: true, Progress: reports}
}

func TestLiveProgressAverageBeforeAnyObjectCompletesAndAfterPartialFailure(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	const large int64 = 350 << 30
	c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: large}, {Key: "small", Size: 1 << 30}})
	sample := childProgressSample(c, 1, 50<<30)
	serveProgressAgent(t, f, c.Node, func(command agenttransport.Command) (any, error) {
		if command.Operation != "task_metrics" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		return progressMetrics(p, sample), nil
	})
	f.master.tasks.sampleRates(context.Background(), p)
	p, err := f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
		if row["putObjectSize"] != int64(0) || row["averageSpeedIncludesInFlight"] != true {
			t.Fatalf("in-flight bytes were credited as completed, or average remained durable-only: %+v", row)
		}
		got, ok := row["averageSpeedBytesPerSecond"].(float64)
		want := float64(sample.InFlightBytes) / time.Since(*p.StartedAt).Seconds()
		if !ok || math.Abs(got-want) > want*0.001 {
			t.Fatalf("large file average = %v, want approximately %v", row["averageSpeedBytesPerSecond"], want)
		}
	}
	// Failed/incomplete bytes must disappear from the final average even while
	// the last live sample remains fresh in the metrics cache.
	finished := time.Now()
	record := *c.Record
	record.Status, record.UpdatedAt, record.FinishedAt = execution.PartialFailed, finished, &finished
	record.ActiveTime = &execution.ActiveTime{ElapsedMillis: finished.Sub(*p.StartedAt).Milliseconds()}
	record.Counters.Succeeded, record.Counters.SucceededBytes = 1, 1<<30
	record.Counters.Failed, record.Counters.FailedBytes = 1, large
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.ScanProgress(p.ID, p.Generation, 2, large+1<<30, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if ready, err := f.master.tasks.repo.FinishIfReady(p.ID, p.Generation); err != nil || !ready {
		t.Fatal("fixture did not finish", ready, err)
	}
	p, err = f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Record = &record
	for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
		got, ok := row["averageSpeedBytesPerSecond"].(float64)
		if !ok || got <= 0 || got > float64(1<<30)/59 || row["averageSpeedIncludesInFlight"] == true {
			t.Fatalf("terminal average retained in-flight bytes: %+v", row)
		}
	}
}

func TestLiveProgressMetricsDoNotDoubleCountCompletionDuringRPC(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	const large int64 = 350 << 30
	c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: large}})
	other := assignProgressChild(t, f, p, 2, []transferstore.Object{{Key: "other", Size: 100}})
	serveProgressAgent(t, f, c.Node, func(command agenttransport.Command) (any, error) {
		if command.Operation != "task_metrics" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		// The Agent took its sample before the normal observer credited completion.
		record := *c.Record
		record.Status, record.UpdatedAt = execution.Completed, time.Now()
		record.Counters.Succeeded, record.Counters.SucceededBytes = 1, large
		if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
			return nil, err
		}
		return progressMetrics(p, childProgressSample(c, 1, large/2), childProgressSample(other, 1, 50)), nil
	})
	f.master.tasks.sampleRates(context.Background(), p)
	p, err := f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry, fresh := f.master.tasks.progressSnapshot(p)
	if !fresh || entry.progress == nil || entry.progress.TotalBytes != large+50 || len(entry.progress.Children) != 1 || entry.progress.Children[0].Progress.ChildID != other.Job.ChildID {
		t.Fatalf("completion was double counted or discarded another active child: %+v", entry.progress)
	}
	row := f.master.taskView(p)
	want := float64(large+50) / time.Since(*p.StartedAt).Seconds()
	if got, ok := row["averageSpeedBytesPerSecond"].(float64); !ok || math.Abs(got-want) > want*0.001 {
		t.Fatalf("average after ledger advance = %v, want approximately %v", row["averageSpeedBytesPerSecond"], want)
	}
}

func TestLiveProgressAverageIsUnknownWhenSupportedSampleExpiresOrRefreshFails(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: 350 << 30}})
	var staleResponse atomic.Bool
	serveProgressAgent(t, f, c.Node, func(command agenttransport.Command) (any, error) {
		if command.Operation != "task_metrics" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		reply := progressMetrics(p, childProgressSample(c, 1, 50<<30))
		if staleResponse.Load() {
			reply.Generation++
		}
		return reply, nil
	})
	f.master.tasks.sampleRates(context.Background(), p)
	original, fresh := f.master.tasks.progressSnapshot(p)
	if !fresh || original.progress == nil {
		t.Fatal("initial live sample missing")
	}
	for _, test := range []struct {
		name   string
		change func(*taskRateCache)
	}{
		{"expired", func(entry *taskRateCache) { entry.observed = time.Now().Add(-4 * time.Second) }},
		{"invalid", func(entry *taskRateCache) { entry.valid = false }},
		{"unavailable projection", func(entry *taskRateCache) { entry.progress = nil }},
		{"generation changed", func(entry *taskRateCache) { entry.generation++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := original
			test.change(&entry)
			f.master.tasks.mu.Lock()
			f.master.tasks.rates[p.ID] = entry
			f.master.tasks.mu.Unlock()
			for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
				if value, exists := row["averageSpeedBytesPerSecond"]; exists {
					t.Fatalf("unavailable sample displayed an average of %v", value)
				}
			}
		})
	}
	// Older Agents deliberately retain their completed-object average. A newer
	// Agent failing to refresh must not be mistaken for that older capability.
	f.master.tasks.mu.Lock()
	legacy := original
	legacy.progressSupported, legacy.progress = false, nil
	f.master.tasks.rates[p.ID] = legacy
	f.master.tasks.mu.Unlock()
	if got := f.master.taskView(p)["averageSpeedBytesPerSecond"]; got != float64(0) {
		t.Fatalf("legacy Agent average changed: %v", got)
	}
	original.attempted = time.Now().Add(-2 * time.Second)
	f.master.tasks.mu.Lock()
	f.master.tasks.rates[p.ID] = original
	f.master.tasks.mu.Unlock()
	staleResponse.Store(true)
	f.master.tasks.sampleRates(context.Background(), p)
	row := f.master.taskView(p)
	if value, exists := row["averageSpeedBytesPerSecond"]; exists {
		t.Fatalf("failed refresh reset supported progress to a misleading average of %v", value)
	}
}

type progressHTTPData struct {
	Supported bool               `json:"supported"`
	Partial   bool               `json:"partial"`
	Items     []fileProgressView `json:"items"`
	Total     int                `json:"total"`
	HasMore   bool               `json:"hasMore"`
}

func getProgressHTTP(t *testing.T, f *agentHTTPFixture, p task.Parent, query string) (int, progressHTTPData) {
	t.Helper()
	status, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/progress%s", f.http.URL, p.ID, query), f.admin, nil)
	raw, err := json.Marshal(body["data"])
	if err != nil {
		t.Fatal(err)
	}
	var data progressHTTPData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return status, data
}

func TestLiveProgressHTTPBoundsDetailsWithoutWritingHistory(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	var objects []transferstore.Object
	var items []execution.ObjectProgress
	for i := range 128 {
		key := fmt.Sprintf("object-%03d", i)
		objects = append(objects, transferstore.Object{Key: key, Size: 1 << 30})
		items = append(items, execution.ObjectProgress{ItemID: key, SourceKey: key, DestinationKey: "copy/" + key, Size: 1 << 30, TransferredBytes: 1 << 29, Stage: "TRANSFERRING"})
	}
	c := assignProgressChild(t, f, p, 1, objects)
	sample := childProgressSample(c, 128, 128<<29)
	serveProgressAgent(t, f, c.Node, func(command agenttransport.Command) (any, error) {
		switch command.Operation {
		case "task_metrics":
			return progressMetrics(p, sample), nil
		case "task_progress":
			var input struct {
				ParentID   int64  `json:"parentId"`
				Generation uint64 `json:"generation"`
				ChildID    string `json:"childId"`
			}
			if err := json.Unmarshal(command.Payload, &input); err != nil {
				return nil, err
			}
			if input.ParentID != p.ID || input.Generation != p.Generation || input.ChildID != c.Job.ChildID {
				return nil, fmt.Errorf("wrong Agent progress request: %+v", input)
			}
			return fileProgressReply{Supported: true, Child: &sample, Items: items}, nil
		default:
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
	})
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		status, parent := getProgressHTTP(t, f, p, "")
		if status != 200 || !parent.Supported || parent.Partial || len(parent.Items) != 20 || parent.Total != 128 || !parent.HasMore {
			t.Fatal("parent preview was not bounded", status, parent)
		}
		status, child := getProgressHTTP(t, f, p, "?childNumber=1&generation=1")
		if status != 200 || child.Partial || len(child.Items) != 128 || child.Total != 128 || child.HasMore {
			t.Fatal("child detail omitted active objects", status, child)
		}
		if item := child.Items[127]; item.ChildTaskID != c.Job.ChildID || item.SubtaskNumber != 1 || item.ExecutionNodeLabel != c.Node.Address || item.TransferredBytes != 1<<29 {
			t.Fatalf("wrong progress or node identity: %+v", item)
		}
	}
	after, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("viewing live progress wrote to the task database", err)
	}
	status, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/progress", f.http.URL, p.ID), "", nil)
	if status != 401 {
		t.Fatalf("file paths were exposed without authentication: HTTP %d", status)
	}
}

func TestLiveProgressHTTPRejectsStaleRepliesAndConcurrentOwnershipChanges(t *testing.T) {
	for _, scenario := range []string{"old-generation-reply", "old-assignment-reply", "resume-during-rpc", "takeover-during-rpc", "oversized-reply"} {
		t.Run(scenario, func(t *testing.T) {
			f, p := progressMasterFixture(t, 2)
			c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: 100}})
			sample := childProgressSample(c, 1, 50)
			for _, node := range p.Spec.Nodes {
				serveProgressAgent(t, f, node, func(command agenttransport.Command) (any, error) {
					if command.Operation == "task_metrics" {
						if node.ID == c.Node.ID {
							return progressMetrics(p, sample), nil
						}
						return progressMetrics(p), nil
					}
					if command.Operation != "task_progress" || node.ID != c.Node.ID {
						return nil, fmt.Errorf("unexpected progress command on %s: %s", node.ID, command.Operation)
					}
					replySample := sample
					files := []execution.ObjectProgress{{ItemID: "old-attempt", SourceKey: "private/old-key", Size: 100, TransferredBytes: 50, Stage: "TRANSFERRING"}}
					switch scenario {
					case "old-generation-reply":
						replySample.Generation++
					case "old-assignment-reply":
						replySample.Assignment++
					case "resume-during-rpc":
						if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
							return nil, err
						}
						if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
							return nil, err
						}
						if _, err := f.master.tasks.repo.Resume(p.ID); err != nil {
							return nil, err
						}
					case "takeover-during-rpc":
						if _, err := f.master.tasks.repo.EnableFailover(p.ID, p.Generation, false); err != nil {
							return nil, err
						}
						if _, err := f.master.tasks.repo.BeginFailover(p.ID, p.Generation, c.Node.ID); err != nil {
							return nil, err
						}
						available := map[string]bool{}
						for _, candidate := range p.Spec.Nodes {
							available[candidate.ID] = candidate.ID != c.Node.ID
						}
						moved, _, err := f.master.tasks.repo.PrepareFailover(p.ID, p.Generation, c.Node.ID, available)
						if err != nil || len(moved) != 1 {
							return nil, fmt.Errorf("takeover fixture failed: moved %d: %w", len(moved), err)
						}
					case "oversized-reply":
						for i := 1; i < 129; i++ {
							file := files[0]
							file.ItemID = fmt.Sprint(i)
							files = append(files, file)
						}
					}
					return fileProgressReply{Supported: true, Child: &replySample, Items: files}, nil
				})
			}
			status, data := getProgressHTTP(t, f, p, "?generation=1")
			wantStatus := 200
			if scenario == "resume-during-rpc" {
				wantStatus = 409
			}
			if status != wantStatus || len(data.Items) != 0 {
				t.Fatalf("obsolete file progress escaped ownership fence: HTTP %d %+v", status, data)
			}
			if status == 200 && (data.Total != 0 || data.HasMore || !data.Partial) {
				t.Fatalf("rejected file progress left a stale total: %+v", data)
			}
		})
	}
}
