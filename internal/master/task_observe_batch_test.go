package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestBatchedObservationPartialPagesMissingAndFences(t *testing.T) {
	for _, scenario := range []string{"partial", "missing", "cancel", "wrong-id", "wrong-generation", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, p, broker := schedulingFixture(t, 1)
			children := assignSchedulingChildren(t, runtime, p, 8)
			records := map[string]execution.Record{}
			for _, child := range children {
				records[child.Job.ChildID] = execution.Record{Job: child.Job, Attempt: 1, Status: execution.Completed, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			observeCtx, stopObserve := context.WithCancel(ctx)
			defer stopObserve()
			var calls, accepts atomic.Int64
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
					var response any
					switch command.Operation {
					case "children_get":
						count := calls.Add(1)
						if scenario == "oversized" {
							err := broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 413, Payload: json.RawMessage(`{"code":"batch_item_too_large"}`)}})
							if err != nil {
								done <- err
								return
							}
							continue
						}
						if scenario == "cancel" && count == 2 {
							stopObserve()
							continue
						}
						var request agentservice.ChildRecordsRequest
						if err := json.Unmarshal(command.Payload, &request); err != nil {
							done <- err
							return
						}
						page := agentservice.ChildRecordsResponse{}
						for i, id := range request.ChildIDs[:min(3, len(request.ChildIDs))] {
							record := records[id]
							row := agentservice.ChildRecord{ChildID: id, Record: &record}
							if scenario == "wrong-id" && i == 1 {
								row.ChildID = "other"
							}
							if scenario == "wrong-generation" && i == 1 {
								record.Job.Generation++
							}
							if scenario == "missing" && id == children[0].Job.ChildID {
								row.Record = nil
								row.Missing = true
							}
							page.Children = append(page.Children, row)
						}
						response = page
					case "child_get":
						var request struct {
							ChildID string `json:"childId"`
						}
						if err := json.Unmarshal(command.Payload, &request); err != nil {
							done <- err
							return
						}
						response = records[request.ChildID]
					case "child_accept":
						if scenario != "missing" {
							done <- errors.New("unexpected accept")
							return
						}
						accepts.Add(1)
						response = map[string]any{}
					default:
						done <- fmt.Errorf("unexpected operation %s", command.Operation)
						return
					}
					payload, _ := json.Marshal(response)
					if err := broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}}); err != nil {
						done <- err
						return
					}
				}
			}()
			err := runtime.observeAvailable(observeCtx, p, 64, map[string]bool{p.Spec.Nodes[0].ID: true})
			cancel()
			serveErr := <-done
			if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
				t.Fatal(serveErr)
			}
			wantFinished := 8
			switch scenario {
			case "partial":
				if err != nil || calls.Load() != 3 {
					t.Fatal("partial page did not continue", calls.Load(), err)
				}
			case "oversized":
				if err != nil || calls.Load() != 1 {
					t.Fatal("large-item fallback failed", calls.Load(), err)
				}
			case "missing":
				wantFinished = 7
				if err != nil || accepts.Load() != 1 {
					t.Fatal("missing assignment not redelivered", accepts.Load(), err)
				}
			case "cancel":
				wantFinished = 3
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation swallowed", err)
				}
			default:
				wantFinished = 0
				if !errors.Is(err, task.ErrConflict) {
					t.Fatal("invalid identity/generation accepted", err)
				}
			}
			current, readErr := runtime.repo.Get(p.ID)
			if readErr != nil || current.FinishedChildren != wantFinished {
				t.Fatal("wrong durable outcomes", current.FinishedChildren, wantFinished, readErr)
			}
		})
	}
}
