package master

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestCollectBatchesPersistsScannerPageCheckpointWithAssignments(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "batched"
		if fallback {
			name = "single-batch-fallback"
		}
		t.Run(name, func(t *testing.T) {
			f, p := progressMasterFixture(t, 1)
			runtime := f.master.tasks
			checkpoint := &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, ThroughBatch: 1, Objects: 1, Bytes: 1024, Cursor: "source-page-position"}
			batch := discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: 1, Objects: []transferstore.Object{{Key: "first", Size: 1024}}, Bytes: 1024, Checkpoint: checkpoint}
			serveScanReleaseAgent(t, f, p.ScannerNode(), func(command agenttransport.Command) (any, int) {
				if command.Operation == "scan_batches" {
					if fallback {
						return map[string]string{"code": "unknown_operation"}, 404
					}
					return agentservice.ScanBatchesResponse{Batches: []discovery.Batch{batch}}, 200
				}
				if command.Operation == "scan_batch" {
					var request agentservice.ScanRequest
					if err := json.Unmarshal(command.Payload, &request); err != nil {
						t.Error(err)
						return nil, 500
					}
					return map[string]any{"found": request.Number == 0, "batch": batch}, 200
				}
				t.Errorf("unexpected operation %s", command.Operation)
				return nil, 500
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			assignments, err := runtime.collectBatches(ctx, p, agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}, 2)
			if err != nil || len(assignments) != 1 {
				t.Fatalf("collect: %+v %v", assignments, err)
			}
			if _, err := runtime.repo.AssignPageForScanner(p.ID, p.Generation, p.ScannerEpoch(), assignments, map[string]bool{p.ScannerNode().ID: true}); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.repo.Get(p.ID)
			if err != nil || saved.ScanCheckpoint == nil || *saved.ScanCheckpoint != *checkpoint || saved.DispatchedThrough != 1 {
				t.Fatalf("wire checkpoint lost before dispatch commit: %+v %v", saved.ScanCheckpoint, err)
			}
		})
	}
}
