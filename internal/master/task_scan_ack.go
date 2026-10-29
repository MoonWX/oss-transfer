package master

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Assignment and acknowledgement are safe before the current installation is
// committed. Destructive release additionally requires the installation gate:
// an installer must not restore a Master backup after deleting newer manifests.
func (t *taskRuntime) reconcileScanAck(ctx context.Context, p task.Parent, scan discovery.State) error {
	current, err := t.scanAckOwner(p, scan)
	if err != nil {
		return err
	}
	through := int64(current.DispatchedThrough)
	if scan.AckedThrough < through {
		var acknowledged discovery.State
		if err := t.server.agentCall(ctx, nodeSession(current.ScannerNode()), "scan_ack", true, agentservice.ScanRequest{
			ParentID: current.ID, Generation: current.Generation, Epoch: current.ScannerEpoch(), Number: through,
		}, &acknowledged); err != nil {
			return err
		}
		scan = acknowledged
	}
	return t.releaseScanPrefix(ctx, current, scan)
}

func (t *taskRuntime) scanAckOwner(p task.Parent, scan discovery.State) (task.Parent, error) {
	current, err := t.repo.Get(p.ID)
	if err != nil {
		return task.Parent{}, err
	}
	if current.Generation != p.Generation || scan.Generation != current.Generation {
		return task.Parent{}, task.ErrGeneration
	}
	if current.Deleted || current.Purged || current.Failover != nil && current.Failover.ScannerSeeding {
		return task.Parent{}, task.ErrState
	}
	if current.ScannerEpoch() != p.ScannerEpoch() || nodeSession(current.ScannerNode()) != nodeSession(p.ScannerNode()) ||
		scan.Spec.ParentID != current.ID || scan.Epoch != current.ScannerEpoch() {
		return task.Parent{}, task.ErrConflict
	}
	// A candidate Master can acknowledge newer assignments and then restore an
	// older installation backup. The retained, unauthorized tail is replayable.
	through := int64(current.DispatchedThrough)
	if scan.AckedThrough < 0 || scan.AckedThrough > scan.LastBatch || through > scan.LastBatch ||
		scan.ReleasedThrough < 0 || scan.ReleasableThrough < scan.ReleasedThrough || scan.ReleasableThrough > through {
		return task.Parent{}, task.ErrConflict
	}
	return current, nil
}

// Older Agents only acknowledge batches. New Agents retain them too until this
// separate authorization is received. Re-read ownership after ACK delivery so
// a generation or scanner change cannot authorize stale work.
func (t *taskRuntime) releaseScanPrefix(ctx context.Context, p task.Parent, scan discovery.State) error {
	if scan.ReleaseProtocol < 1 {
		return nil
	}
	allowed := t.server.options.ScanReleaseAllowed
	if allowed != nil && !allowed() {
		return nil
	}
	current, err := t.scanAckOwner(p, scan)
	if err != nil {
		return err
	}
	through := int64(current.DispatchedThrough)
	if scan.AckedThrough < through || scan.ReleasableThrough > scan.AckedThrough {
		return task.ErrConflict
	}
	if scan.ReleasableThrough >= through {
		return nil
	}
	return t.server.agentCall(ctx, nodeSession(current.ScannerNode()), "scan_release", true, agentservice.ScanRequest{
		ParentID: current.ID, Generation: current.Generation, Epoch: current.ScannerEpoch(), Number: through,
	}, nil)
}
