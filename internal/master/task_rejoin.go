package master

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Rejoining fences the old queue before restoring membership under a fresh
// epoch. Queue rebalancing may then move work which has not started; active
// transfers on healthy replacements retain their assignments.
func (t *taskRuntime) rejoinAvailableNodes(ctx context.Context, p task.Parent) (task.Parent, error) {
	if p.Failover == nil || !p.Failover.Enabled || len(p.Failover.Migrations) > 0 || p.Failover.ScannerSeeding {
		return p, nil
	}
	online, err := t.server.onlineAgents()
	if err != nil {
		return p, nil
	}
	members := map[string]string{}
	for _, m := range online {
		members[m.ID] = m.EnrollmentID
	}
	for _, node := range p.Spec.Nodes {
		if !p.NodeRetired(node.ID) || !p.CanRejoinScanner(node.ID) || members[node.ID] != node.EnrollmentID {
			continue
		}
		maintenance, maintenanceErr := t.server.agentUnderMaintenance(node.ID)
		if maintenanceErr != nil || maintenance {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var caps struct {
			LeaseProtocol int `json:"leaseProtocol"`
		}
		err = t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
		if err == nil && caps.LeaseProtocol >= 2 {
			err = t.verifyTaskStorage(callCtx, p, []task.Node{node}, false)
		} else {
			cancel()
			continue
		}
		if err == nil {
			var epoch uint64
			p, epoch, err = t.repo.BeginNodeRejoin(p.ID, p.Generation, node.ID)
			if err == nil {
				err = t.leases.rejoin(callCtx, p, node, epoch)
			}
			if err == nil {
				p, err = t.repo.FinishNodeRejoin(p.ID, p.Generation, node.ID, epoch)
			}
		}
		cancel()
		// A returning node can fail again during this handshake. Keep the healthy
		// scanner and transfer owners moving while durable intent is retried.
		if err != nil {
			current, readErr := t.repo.Get(p.ID)
			if readErr != nil {
				return p, readErr
			}
			p = current
			if ctx.Err() != nil {
				return p, ctx.Err()
			}
		}
	}
	return p, nil
}

func (l *taskLeases) rejoin(ctx context.Context, p task.Parent, node task.Node, epoch uint64) error {
	e, err := l.entry(p, node)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current, err := l.runtime.repo.Get(p.ID)
	if err != nil {
		return err
	}
	if current.Generation != p.Generation || !current.NodeRetired(node.ID) || current.Failover.Rejoining[node.ID] != epoch || epoch == 0 {
		return task.ErrState
	}
	if e.record.OwnerEpoch > epoch {
		return errors.New("stale node rejoin")
	}
	var ack struct {
		OwnerEpoch uint64 `json:"ownerEpoch"`
	}
	request := agentservice.LeaseRejoinRequest{ParentID: p.ID, Generation: p.Generation, OwnerEpoch: epoch}
	if p.ScannerNode().ID == node.ID {
		scanEpoch := p.ScannerEpoch()
		request.ScannerEpoch, request.ScannerThrough = &scanEpoch, int64(p.DispatchedThrough)
	}
	err = l.call(ctx, nodeSession(node), "lease_rejoin", true, request, &ack)
	if err != nil {
		return err
	}
	if ack.OwnerEpoch != epoch {
		return errors.New("invalid node rejoin acknowledgement")
	}
	old := e.record
	// The Agent acknowledged that this epoch has no live authority. If it
	// disappears before the first grant, no extra lease wait is necessary.
	fencedAt := l.now()
	e.record = taskLeaseRecord{ParentID: p.ID, Node: node, Generation: p.Generation, OwnerEpoch: epoch, Armed: true, Deadline: fencedAt.UTC()}
	if err = l.save(e); err != nil {
		e.record = old
		return err
	}
	e.safeAfter, e.renewAt = fencedAt, time.Time{}
	e.retireAck = false
	wakeLease(e)
	return nil
}
