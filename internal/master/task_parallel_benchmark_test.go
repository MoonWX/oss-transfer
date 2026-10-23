package master

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Isolate control-plane latency with the real broker and durable assignments.
// Five milliseconds per command models network/handler latency, not object IO.
// The serial variant is the previous Master's identical per-child RPC loop.
func BenchmarkTaskDispatchControlPlane(b *testing.B) {
	for _, nodes := range []int{1, 4} {
		for _, parallel := range []bool{false, true} {
			b.Run(fmt.Sprintf("nodes=%d/parallel=%v", nodes, parallel), func(b *testing.B) {
				runtime, p, broker := schedulingFixture(b, nodes)
				children := assignSchedulingChildren(b, runtime, p, 16)
				ctx, cancel := context.WithCancel(context.Background())
				var workers sync.WaitGroup
				var calls atomic.Int64
				for _, node := range p.Spec.Nodes {
					workers.Add(1)
					go func() {
						defer workers.Done()
						for {
							command, err := broker.Poll(ctx, nodeSession(node))
							if err != nil {
								return
							}
							if err := broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
								return
							}
							timer := time.NewTimer(5 * time.Millisecond)
							select {
							case <-ctx.Done():
								timer.Stop()
								return
							case <-timer.C:
							}
							calls.Add(1)
							_ = broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: json.RawMessage(`{}`)}})
						}
					}()
				}
				defer func() { cancel(); workers.Wait() }()
				b.ResetTimer()
				for range b.N {
					if parallel {
						if err := runtime.deliverChildren(ctx, p, children); err != nil {
							b.Fatal(err)
						}
					} else {
						for _, child := range children {
							if err := runtime.server.agentCall(ctx, nodeSession(child.Node), "child_accept", true, agentservice.ChildAcceptRequest{Job: child.Job}, nil); err != nil {
								b.Fatal(err)
							}
						}
					}
				}
				b.StopTimer()
				if calls.Load() != int64(b.N*16) {
					b.Fatal("command count changed", calls.Load())
				}
				b.ReportMetric(float64(calls.Load())/float64(b.N), "commands/op")
			})
		}
	}
}

func BenchmarkTaskScanControlPlane(b *testing.B) {
	for _, batched := range []bool{false, true} {
		b.Run(fmt.Sprintf("batched=%v", batched), func(b *testing.B) {
			runtime, p, broker := schedulingFixture(b, 1)
			ctx, cancel := context.WithCancel(context.Background())
			var calls atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					command, err := broker.Poll(ctx, nodeSession(p.Spec.Nodes[0]))
					if err != nil {
						return
					}
					if err := broker.Confirm(nodeSession(p.Spec.Nodes[0]), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
						return
					}
					timer := time.NewTimer(5 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					var response any
					var request agentservice.ScanBatchesRequest
					_ = json.Unmarshal(command.Payload, &request)
					batch := func(number int64) discovery.Batch {
						return discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: number, Objects: []transferstore.Object{{Key: fmt.Sprint(number), Size: 1}}}
					}
					if command.Operation == "scan_batch" {
						response = map[string]any{"found": true, "batch": batch(request.Number + 1)}
					} else {
						page := agentservice.ScanBatchesResponse{}
						for i := range request.Limit {
							page.Batches = append(page.Batches, batch(request.Number+int64(i)+1))
						}
						response = page
					}
					calls.Add(1)
					payload, _ := json.Marshal(response)
					_ = broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}})
				}
			}()
			defer func() { cancel(); <-done }()
			b.ResetTimer()
			for range b.N {
				control := agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}
				if batched {
					page, err := runtime.collectBatches(ctx, p, control, 16)
					if err != nil || len(page) != 16 {
						b.Fatal(len(page), err)
					}
				} else {
					page, err := runtime.collectSingleBatches(ctx, p, control, 16)
					if err != nil || len(page) != 16 {
						b.Fatal(len(page), err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(calls.Load())/float64(b.N), "commands/op")
		})
	}
}

func BenchmarkTaskObserveControlPlane(b *testing.B) {
	for _, batched := range []bool{false, true} {
		b.Run(fmt.Sprintf("batched=%v", batched), func(b *testing.B) {
			runtime, p, broker := schedulingFixture(b, 1)
			children := assignSchedulingChildren(b, runtime, p, 16)
			records := map[string]execution.Record{}
			for _, child := range children {
				records[child.Job.ChildID] = execution.Record{Job: child.Job, Attempt: 1, Status: execution.Running, Counters: execution.Counters{Total: 1, TotalBytes: 1}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var calls atomic.Int64
			go func() {
				defer close(done)
				for {
					command, err := broker.Poll(ctx, nodeSession(p.Spec.Nodes[0]))
					if err != nil {
						return
					}
					if err := broker.Confirm(nodeSession(p.Spec.Nodes[0]), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
						return
					}
					timer := time.NewTimer(5 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					var response any
					if command.Operation == "child_get" {
						var request struct {
							ChildID string `json:"childId"`
						}
						_ = json.Unmarshal(command.Payload, &request)
						response = records[request.ChildID]
					} else {
						var request agentservice.ChildRecordsRequest
						_ = json.Unmarshal(command.Payload, &request)
						page := agentservice.ChildRecordsResponse{}
						for _, id := range request.ChildIDs {
							record := records[id]
							page.Children = append(page.Children, agentservice.ChildRecord{ChildID: id, Record: &record})
						}
						response = page
					}
					payload, _ := json.Marshal(response)
					calls.Add(1)
					_ = broker.Complete(nodeSession(p.Spec.Nodes[0]), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: payload}})
				}
			}()
			defer func() { cancel(); <-done }()
			b.ResetTimer()
			for range b.N {
				if batched {
					rows, err := runtime.observeNode(ctx, p, children, true)
					if err != nil || len(rows) != 16 {
						b.Fatal(len(rows), err)
					}
				} else {
					for _, child := range children {
						row, err := runtime.observeOne(ctx, p, child, nil)
						if err != nil || row == nil {
							b.Fatal(err)
						}
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(calls.Load())/float64(b.N), "commands/op")
		})
	}
}
