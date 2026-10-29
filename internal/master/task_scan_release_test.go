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
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func serveScanReleaseAgent(t *testing.T, f *agentHTTPFixture, node task.Node, respond func(agenttransport.Command) (any, int)) {
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
			value, status := respond(command)
			payload, err := json.Marshal(value)
			if err == nil {
				err = f.master.agents.broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: status, Payload: payload}})
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

func releaseState(p task.Parent, through, releasable int64) discovery.State {
	return discovery.State{Spec: discovery.Spec{ParentID: p.ID}, Generation: p.Generation, Epoch: p.ScannerEpoch(),
		LastBatch: 2, AckedThrough: through, ReleasableThrough: releasable, ReleaseProtocol: 1}
}

func TestScanReleaseWaitsForInstallationCommitWithoutBlockingAcknowledgement(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	runtime := f.master.tasks
	assignSchedulingChildren(t, runtime, p, 2)
	var committed atomic.Bool
	f.master.options.ScanReleaseAllowed = committed.Load
	var acked, released, ackCalls, releaseCalls atomic.Int64
	serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
		var request agentservice.ScanRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			t.Error(err)
			return nil, 500
		}
		if request.ParentID != p.ID || request.Generation != p.Generation || request.Number != 2 {
			t.Errorf("wrong prefix: %+v", request)
			return nil, 500
		}
		switch command.Operation {
		case "scan_ack":
			acked.Store(request.Number)
			ackCalls.Add(1)
		case "scan_release":
			if !committed.Load() || acked.Load() != 2 {
				t.Error("release preceded commit or ACK")
				return nil, 500
			}
			released.Store(request.Number)
			releaseCalls.Add(1)
		default:
			t.Errorf("unexpected operation %s", command.Operation)
			return nil, 500
		}
		return releaseState(p, acked.Load(), released.Load()), 200
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if ackCalls.Load() != 1 || releaseCalls.Load() != 0 {
		t.Fatal("uncommitted installation released data", ackCalls.Load(), releaseCalls.Load())
	}
	committed.Store(true)
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if ackCalls.Load() != 1 || released.Load() != 2 {
		t.Fatal("committed installation did not release durable prefix")
	}
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 2, 2)); err != nil {
		t.Fatal(err)
	}
	if releaseCalls.Load() != 1 {
		t.Fatal("unchanged authorization sent redundant release")
	}
}

func TestScanReleaseRequiresConfirmedCurrentAcknowledgement(t *testing.T) {
	for _, scenario := range []string{"ack-failure", "generation-change", "unconfirmed-ack", "legacy-agent"} {
		t.Run(scenario, func(t *testing.T) {
			f, p := progressMasterFixture(t, 1)
			runtime := f.master.tasks
			assignSchedulingChildren(t, runtime, p, 2)
			var releaseCalls atomic.Int64
			serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
				if command.Operation == "scan_release" {
					releaseCalls.Add(1)
					return nil, 500
				}
				if command.Operation != "scan_ack" {
					t.Errorf("unexpected operation %s", command.Operation)
					return nil, 500
				}
				response := releaseState(p, 2, 0)
				switch scenario {
				case "ack-failure":
					return map[string]string{"message": "ACK failed"}, 503
				case "generation-change":
					current, err := runtime.repo.Get(p.ID)
					if err != nil {
						t.Error(err)
						return nil, 500
					}
					current.Generation++
					if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), current) }); err != nil {
						t.Error(err)
						return nil, 500
					}
				case "unconfirmed-ack":
					response.AckedThrough = 1
				case "legacy-agent":
					response.ReleaseProtocol = 0
				}
				return response, 200
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := runtime.reconcileScanAck(ctx, p, releaseState(p, 0, 0))
			if scenario == "legacy-agent" && err != nil {
				t.Fatal("old Agent migration was blocked", err)
			}
			if scenario != "legacy-agent" && err == nil {
				t.Fatal("failed or stale ACK was accepted")
			}
			if releaseCalls.Load() != 0 {
				t.Fatal("unsafe release was sent")
			}
		})
	}
}

func TestDispatchAuthorizesReleaseImmediatelyAfterFinalAcknowledgement(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	runtime := f.master.tasks
	var acknowledged, released atomic.Bool
	serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
		switch command.Operation {
		case "scan_batch":
			return map[string]any{"found": true, "batch": discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: 1, Objects: []transferstore.Object{{Key: "last-object", Size: 1024}}, Bytes: 1024}}, 200
		case "child_accept":
			current, err := runtime.repo.Get(p.ID)
			if err != nil || current.DispatchedThrough != 1 {
				t.Error("child preceded durable assignment", err)
				return nil, 500
			}
			return map[string]any{}, 200
		case "scan_ack":
			acknowledged.Store(true)
			return releaseState(p, 1, 0), 200
		case "scan_release":
			var request agentservice.ScanRequest
			if err := json.Unmarshal(command.Payload, &request); err != nil {
				t.Error(err)
				return nil, 500
			}
			if !acknowledged.Load() || request.Number != 1 {
				t.Error("release preceded confirmed final ACK")
				return nil, 500
			}
			released.Store(true)
			return map[string]any{}, 200
		default:
			t.Errorf("unexpected operation %s", command.Operation)
			return nil, 500
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.dispatch(ctx, p, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}); err != nil {
		t.Fatal(err)
	}
	if !released.Load() {
		t.Fatal("final page requires another task step before release")
	}
}

func TestScanReleaseRetriesLostAuthorizationWithoutAcknowledgingAgain(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	runtime := f.master.tasks
	assignSchedulingChildren(t, runtime, p, 2)
	var releases atomic.Int64
	serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
		if command.Operation != "scan_release" {
			t.Errorf("redundant operation %s", command.Operation)
			return nil, 500
		}
		if releases.Add(1) == 1 {
			return map[string]string{"message": "release response lost"}, 503
		}
		return releaseState(p, 2, 2), 200
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 2, 0)); err == nil {
		t.Fatal("failed release was hidden")
	}
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 2, 0)); err != nil {
		t.Fatal("release retry failed", err)
	}
	if releases.Load() != 2 {
		t.Fatal("release was not retried", releases.Load())
	}
}

func TestScanReleaseReplaysUnreleasedTailAfterMasterDatabaseRollback(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	runtime := f.master.tasks
	assignSchedulingChildren(t, runtime, p, 1) // Restored snapshot only contains batch 1.
	p, err := runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var releasable, reads atomic.Int64
	serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
		switch command.Operation {
		case "scan_release":
			var request agentservice.ScanRequest
			if err := json.Unmarshal(command.Payload, &request); err != nil {
				t.Error(err)
				return nil, 500
			}
			persisted, err := runtime.repo.Get(p.ID)
			if err != nil || request.Number != int64(persisted.DispatchedThrough) {
				t.Error("authorized candidate-only assignments", request.Number, err)
				return nil, 500
			}
			releasable.Store(request.Number)
			return releaseState(p, 2, request.Number), 200
		case "scan_batches":
			var request agentservice.ScanBatchesRequest
			if err := json.Unmarshal(command.Payload, &request); err != nil {
				t.Error(err)
				return nil, 500
			}
			if request.Number != 1 || releasable.Load() != 1 {
				t.Error("did not replay retained tail from restored cursor")
				return nil, 500
			}
			reads.Add(1)
			return agentservice.ScanBatchesResponse{Batches: []discovery.Batch{{ParentID: p.ID, Generation: p.Generation, Number: 2, Objects: []transferstore.Object{{Key: "candidate-only-object", Size: 4096}}, Bytes: 4096}}}, 200
		case "child_accept":
			return map[string]any{}, 200
		case "scan_ack":
			return releaseState(p, 2, releasable.Load()), 200
		default:
			t.Errorf("unexpected operation %s", command.Operation)
			return nil, 500
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Candidate acknowledged batch 2, but never authorized its deletion.
	if err := runtime.reconcileScanAck(ctx, p, releaseState(p, 2, 0)); err != nil {
		t.Fatal("safe rollback prefix was rejected", err)
	}
	if releasable.Load() != 1 {
		t.Fatal("release exceeded restored durable assignments", releasable.Load())
	}
	if err := runtime.dispatch(ctx, p, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}); err != nil {
		t.Fatal("retained tail could not be replayed", err)
	}
	if reads.Load() != 1 || releasable.Load() != 2 {
		t.Fatal("tail was not persisted before its release")
	}
	children, err := runtime.repo.Children(p.ID, 1, 1)
	if err != nil || len(children) != 1 || children[0].Job.Number != 2 || children[0].Job.Items[0].Source.Key != "candidate-only-object" {
		t.Fatal("replayed tail not durable", children, err)
	}
}
