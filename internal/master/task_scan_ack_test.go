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
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestTaskStepAcknowledgesDurablePrefixWithFullDispatchQueue(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	runtime := f.master.tasks
	assignSchedulingChildren(t, runtime, p, 64)
	p, err := runtime.repo.Get(p.ID)
	if err != nil || pendingDispatchCapacity(p) != 0 {
		t.Fatal("fixture must saturate the dispatch queue", p, err)
	}
	var acknowledged, calls atomic.Int64
	serveProgressAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, error) {
		switch command.Operation {
		case "storage_apply":
			var snapshot storageconfig.Snapshot
			if err := json.Unmarshal(command.Payload, &snapshot); err != nil {
				return nil, err
			}
			return map[string]any{"revision": snapshot.Revision}, nil
		case "bandwidth_parent", "child_accept":
			return map[string]any{}, nil
		case "children_get":
			var request agentservice.ChildRecordsRequest
			if err := json.Unmarshal(command.Payload, &request); err != nil {
				return nil, err
			}
			page := agentservice.ChildRecordsResponse{}
			for _, id := range request.ChildIDs {
				page.Children = append(page.Children, agentservice.ChildRecord{ChildID: id, Missing: true})
			}
			return page, nil
		case "scan_start":
			return discovery.State{Spec: runtime.scanSpec(p), Generation: p.Generation, LastBatch: 65,
				AckedThrough: acknowledged.Load(), TotalObjects: 65, TotalBytes: 65, ScanComplete: true}, nil
		case "scan_ack":
			var request agentservice.ScanRequest
			if err := json.Unmarshal(command.Payload, &request); err != nil {
				return nil, err
			}
			if request.ParentID != p.ID || request.Generation != p.Generation || request.Epoch != p.ScannerEpoch() {
				return nil, fmt.Errorf("wrong ACK identity: %+v", request)
			}
			acknowledged.Store(request.Number)
			calls.Add(1)
			return map[string]any{}, nil
		case "task_metrics":
			return progressMetrics(p), nil
		default:
			return nil, fmt.Errorf("full queue must not read another page: %s", command.Operation)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		if err := runtime.step(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if acknowledged.Load() != 64 || calls.Load() != 1 {
		t.Fatal("queue backpressure retained an assigned page or ACKed unassigned batch 65", acknowledged.Load(), calls.Load())
	}
	current, err := runtime.repo.Get(p.ID)
	if err != nil || current.DispatchedThrough != 64 || current.ScannedBatches != 65 || current.ChildCount != 64 {
		t.Fatal("ACK reconciliation altered assignment progress", current, err)
	}
}

func TestScanAcknowledgementRejectsWrongOwnershipAndUnpersistedRanges(t *testing.T) {
	runtime, p, _ := schedulingFixture(t, 2)
	assignSchedulingChildren(t, runtime, p, 2)
	p, err := runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	valid := discovery.State{Spec: discovery.Spec{ParentID: p.ID}, Generation: p.Generation, LastBatch: 3, AckedThrough: 1}
	for _, test := range []struct {
		name string
		edit func(*task.Parent, *discovery.State)
		want error
	}{
		{"response-generation", func(_ *task.Parent, s *discovery.State) { s.Generation++ }, task.ErrGeneration},
		{"stale-parent-generation", func(p *task.Parent, _ *discovery.State) { p.Generation++ }, task.ErrGeneration},
		{"response-parent", func(_ *task.Parent, s *discovery.State) { s.Spec.ParentID++ }, task.ErrConflict},
		{"response-epoch", func(_ *task.Parent, s *discovery.State) { s.Epoch++ }, task.ErrConflict},
		{"stale-scanner", func(p *task.Parent, _ *discovery.State) { p.Spec.Nodes = []task.Node{p.Spec.Nodes[1]} }, task.ErrConflict},
		{"replacement-enrollment", func(p *task.Parent, _ *discovery.State) { p.Spec.Nodes[0].EnrollmentID = "replacement" }, task.ErrConflict},
		{"missing-persisted-batch", func(_ *task.Parent, s *discovery.State) { s.LastBatch = 1 }, task.ErrConflict},
		{"ack-ahead-of-scanner", func(_ *task.Parent, s *discovery.State) { s.AckedThrough = 4 }, task.ErrConflict},
		{"released-ahead-of-master", func(_ *task.Parent, s *discovery.State) { s.ReleasedThrough = 3; s.ReleasableThrough = 3 }, task.ErrConflict},
		{"authorized-ahead-of-master", func(_ *task.Parent, s *discovery.State) { s.ReleasableThrough = 3 }, task.ErrConflict},
		{"negative-release", func(_ *task.Parent, s *discovery.State) { s.ReleasedThrough = -1 }, task.ErrConflict},
		{"release-beyond-authorization", func(_ *task.Parent, s *discovery.State) { s.ReleasedThrough = 1 }, task.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, scan := p, valid
			parent.Spec.Nodes = append([]task.Node(nil), p.Spec.Nodes...)
			test.edit(&parent, &scan)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := runtime.reconcileScanAck(ctx, parent, scan); !errors.Is(err, test.want) {
				t.Fatal("unsafe ACK was not rejected before delivery", err)
			}
		})
	}
}

func TestScanAcknowledgementUsesCurrentTakeoverScannerEpoch(t *testing.T) {
	f, p := progressMasterFixture(t, 2)
	runtime := f.master.tasks
	assignSchedulingChildren(t, runtime, p, 2)
	p, err := runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := p
	p.Failover = &task.FailoverState{Enabled: true, ScannerNodeID: p.Spec.Nodes[1].ID, ScannerEpoch: 1}
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), p) }); err != nil {
		t.Fatal(err)
	}
	scan := discovery.State{Spec: runtime.scanSpec(p), Generation: p.Generation, Epoch: 1, LastBatch: 3, AckedThrough: 1}
	if err := runtime.reconcileScanAck(context.Background(), old, scan); !errors.Is(err, task.ErrConflict) {
		t.Fatal("stale scanner snapshot survived takeover", err)
	}
	var acknowledged atomic.Int64
	serveProgressAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, error) {
		if command.Operation != "scan_ack" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		var request agentservice.ScanRequest
		if err := json.Unmarshal(command.Payload, &request); err != nil {
			return nil, err
		}
		if request.ParentID != p.ID || request.Generation != p.Generation || request.Epoch != 1 {
			return nil, fmt.Errorf("wrong takeover ACK: %+v", request)
		}
		acknowledged.Store(request.Number)
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.reconcileScanAck(ctx, p, scan); err != nil || acknowledged.Load() != 2 {
		t.Fatal("takeover scanner did not receive the persisted prefix", err, acknowledged.Load())
	}
}
