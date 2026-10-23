package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func schedulingFixture(t testing.TB, nodes int) (*taskRuntime, task.Parent, *agenttransport.Broker) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	broker := agenttransport.NewBroker(agenttransport.BrokerOptions{})
	runtime := newTaskRuntime(&Server{store: db, agents: &agentControl{broker: broker}})
	t.Cleanup(func() { runtime.close(); broker.Close(); db.Close() })
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	for i := range nodes {
		node := task.Node{ID: fmt.Sprint(i), EnrollmentID: "enrollment"}
		spec.Nodes = append(spec.Nodes, node)
		if err := broker.Activate(nodeSession(node)); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := runtime.repo.Create(spec, "scheduling")
	if err != nil {
		t.Fatal(err)
	}
	return runtime, p, broker
}

func assignSchedulingChildren(t testing.TB, runtime *taskRuntime, p task.Parent, count int) []task.Child {
	t.Helper()
	var all []task.Child
	for start := 1; start <= count; start += 16 {
		var page []task.Assignment
		for n := start; n < start+16 && n <= count; n++ {
			page = append(page, task.Assignment{Number: n, Objects: []transferstore.Object{{Key: fmt.Sprint(n), Size: 1}}})
		}
		children, err := runtime.repo.AssignPage(p.ID, p.Generation, page)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, children...)
	}
	return all
}

func TestObservationRotatesPastSlowChildrenAndUnavailableFirstChild(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-unavailable=%v", unavailable), func(t *testing.T) {
			runtime, p, broker := schedulingFixture(t, 1)
			children := assignSchedulingChildren(t, runtime, p, 80)
			reports := map[string]execution.Record{}
			for _, child := range children {
				record := execution.Record{Job: child.Job, Attempt: 1, Status: execution.Running, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1}}
				if child.Job.Number > 64 {
					record.Status = execution.Completed
					record.Counters.Succeeded, record.Counters.SucceededBytes = 1, 1
				}
				reports[child.Job.ChildID] = record
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				for {
					command, err := broker.Poll(ctx, nodeSession(p.Spec.Nodes[0]))
					if err != nil {
						done <- err
						return
					}
					if err := broker.Confirm(nodeSession(p.Spec.Nodes[0]), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
						done <- err
						return
					}
					if command.Operation == "children_get" {
						if err := broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}}); err != nil {
							done <- err
							return
						}
						continue
					}
					var request map[string]string
					if command.Operation != "child_get" {
						done <- fmt.Errorf("unexpected operation %s", command.Operation)
						return
					}
					if err := json.Unmarshal(command.Payload, &request); err != nil {
						done <- err
						return
					}
					status := 200
					var value any = reports[request["childId"]]
					if unavailable && request["childId"] == children[0].Job.ChildID {
						status, value = 503, map[string]string{"message": "unavailable"}
					}
					payload, err := json.Marshal(value)
					if err == nil {
						err = broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: status, Payload: payload}})
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
			if err := runtime.observe(ctx, p, 64); (err != nil) != unavailable {
				t.Fatalf("first observation: %v", err)
			}
			if err := runtime.observe(ctx, p, 64); err != nil {
				t.Fatal(err)
			}
			current, err := runtime.repo.Get(p.ID)
			if err != nil || current.FinishedChildren == 0 {
				t.Fatalf("slow first page prevented later completed children from being observed: %+v %v", current, err)
			}
		})
	}
}

func TestDispatchCapacityUsesEntireTaskAbove256OutstandingChildren(t *testing.T) {
	runtime, p, _ := schedulingFixture(t, 5)
	assignSchedulingChildren(t, runtime, p, 320)
	p, err := runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// No Agent polls the broker. At capacity, dispatch must return without
	// asking the scanner for more work, even though a page returns only 256.
	if err := runtime.dispatch(ctx, p, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}); err != nil {
		t.Fatalf("full queue tried to fetch or dispatch more work: %v", err)
	}
	current, err := runtime.repo.Get(p.ID)
	if err != nil || current.ChildCount != 320 {
		t.Fatal("full queue changed assignments", err)
	}
}
