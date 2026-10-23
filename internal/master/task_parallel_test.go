package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Use the command broker and persisted assignments. The first node cannot
// reply until the second node has actually received its own commands.
func TestParallelNodesProgressWhileFirstNodeWaits(t *testing.T) {
	for _, operation := range []string{"deliver", "observe"} {
		t.Run(operation, func(t *testing.T) {
			runtime, p, broker := schedulingFixture(t, 2)
			children := assignSchedulingChildren(t, runtime, p, 8)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			healthyDone := make(chan struct{})
			var workers sync.WaitGroup
			failures := make(chan error, 2)
			for nodeIndex, node := range p.Spec.Nodes {
				workers.Add(1)
				go func() {
					defer workers.Done()
					seen := 0
					for {
						command, err := broker.Poll(ctx, nodeSession(node))
						if err != nil {
							if !errors.Is(err, context.Canceled) {
								failures <- err
							}
							return
						}
						if err := broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
							failures <- err
							return
						}
						if command.Operation == "children_get" {
							if err := broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}}); err != nil {
								failures <- err
								return
							}
							continue
						}
						if nodeIndex == 0 {
							select {
							case <-healthyDone:
							case <-ctx.Done():
								failures <- errors.New("healthy node was blocked by the first node")
								return
							}
						}
						var payload []byte
						if operation == "deliver" {
							var request agentservice.ChildAcceptRequest
							if command.Operation != "child_accept" {
								failures <- fmt.Errorf("operation %s", command.Operation)
								return
							}
							if err := json.Unmarshal(command.Payload, &request); err != nil {
								failures <- err
								return
							}
							rows, err := runtime.repo.Children(p.ID, request.Job.Number-1, 1)
							if err != nil || len(rows) != 1 || rows[0].Node != node {
								failures <- errors.New("delivery did not use the persisted assignment")
								return
							}
							payload = []byte(`{}`)
						} else {
							var request struct {
								ChildID string `json:"childId"`
							}
							if command.Operation != "child_get" {
								failures <- fmt.Errorf("operation %s", command.Operation)
								return
							}
							if err := json.Unmarshal(command.Payload, &request); err != nil {
								failures <- err
								return
							}
							var child task.Child
							for _, candidate := range children {
								if candidate.Job.ChildID == request.ChildID {
									child = candidate
									break
								}
							}
							payload, _ = json.Marshal(execution.Record{Job: child.Job, Attempt: 1, Status: execution.Completed, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1}})
						}
						if err := broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}}); err != nil {
							failures <- err
							return
						}
						seen++
						if nodeIndex == 1 && seen == 4 {
							close(healthyDone)
						}
					}
				}()
			}
			var err error
			if operation == "deliver" {
				err = runtime.deliverChildren(ctx, p, children)
			} else {
				err = runtime.observeAvailable(ctx, p, 64, map[string]bool{p.Spec.Nodes[0].ID: true, p.Spec.Nodes[1].ID: true})
			}
			cancel()
			workers.Wait()
			close(failures)
			if err != nil {
				t.Fatal(err)
			}
			for failure := range failures {
				t.Error(failure)
			}
			if operation == "observe" {
				current, err := runtime.repo.Get(p.ID)
				if err != nil || current.FinishedChildren != 8 || current.Counters.Succeeded != 8 {
					t.Fatal("parallel reports lost outcomes", current, err)
				}
			}
		})
	}
}

func TestBatchCollectionValidatesPagesAndUsesOneCommand(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			runtime, p, broker := schedulingFixture(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				command, err := broker.Poll(ctx, nodeSession(p.Spec.Nodes[0]))
				if err != nil {
					done <- err
					return
				}
				if command.Operation != "scan_batches" {
					done <- fmt.Errorf("operation %s", command.Operation)
					return
				}
				if err = broker.Confirm(nodeSession(p.Spec.Nodes[0]), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
					done <- err
					return
				}
				result := agentservice.ScanBatchesResponse{}
				for i := 1; i <= 16; i++ {
					result.Batches = append(result.Batches, discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: int64(i), Objects: []transferstore.Object{{Key: fmt.Sprint(i), Size: 1}}})
				}
				if invalid {
					result.Batches[3].Generation++
				}
				payload, _ := json.Marshal(result)
				done <- broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}})
			}()
			batches, err := runtime.collectBatches(ctx, p, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}, 16)
			if invalid {
				if !errors.Is(err, task.ErrConflict) || len(batches) != 3 {
					t.Fatal("generation fence not enforced", len(batches), err)
				}
			} else if err != nil || len(batches) != 16 {
				t.Fatal(len(batches), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
