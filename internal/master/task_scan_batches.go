package master

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func (t *taskRuntime) collectBatches(ctx context.Context, p task.Parent, control agentservice.ScanRequest, limit int) ([]task.Assignment, error) {
	scanner := nodeSession(p.ScannerNode())
	var batches []discovery.Batch
	if limit > 1 {
		var result agentservice.ScanBatchesResponse
		err := t.server.agentCall(ctx, scanner, "scan_batches", false, agentservice.ScanBatchesRequest{ScanRequest: control, Limit: limit}, &result)
		if err == nil {
			if len(result.Batches) > limit {
				return nil, task.ErrConflict
			}
			batches = result.Batches
		} else {
			if !batchReadFallback(err) {
				return nil, err
			}
			// Older Agents retain the existing one-batch protocol during an upgrade.
			return t.collectSingleBatches(ctx, p, control, limit)
		}
	} else {
		return t.collectSingleBatches(ctx, p, control, limit)
	}
	assignments := make([]task.Assignment, 0, len(batches))
	for _, batch := range batches {
		if batch.ParentID != p.ID || batch.Generation != p.Generation || batch.Epoch != p.ScannerEpoch() || batch.Number != control.Number+1 {
			return assignments, task.ErrConflict
		}
		assignments = append(assignments, task.Assignment{Checkpoint: batch.Checkpoint, Number: int(batch.Number), Objects: batch.Objects, Legacy: batch.Legacy})
		control.Number = batch.Number
	}
	return assignments, nil
}

func (t *taskRuntime) collectSingleBatches(ctx context.Context, p task.Parent, control agentservice.ScanRequest, limit int) ([]task.Assignment, error) {
	batches := make([]task.Assignment, 0, limit)
	for range limit {
		var page struct {
			Batch discovery.Batch `json:"batch"`
			Found bool            `json:"found"`
		}
		if err := t.server.agentCall(ctx, nodeSession(p.ScannerNode()), "scan_batch", false, control, &page); err != nil {
			return batches, err
		}
		if !page.Found {
			break
		}
		if page.Batch.ParentID != p.ID || page.Batch.Generation != p.Generation || page.Batch.Epoch != p.ScannerEpoch() || page.Batch.Number != control.Number+1 {
			return batches, task.ErrConflict
		}
		batches = append(batches, task.Assignment{Checkpoint: page.Batch.Checkpoint, Number: int(page.Batch.Number), Objects: page.Batch.Objects, Legacy: page.Batch.Legacy})
		control.Number = page.Batch.Number
	}
	return batches, nil
}
