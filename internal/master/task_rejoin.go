package master

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Rejoining fences the old queue before restoring membership under a fresh
// epoch. Queue rebalancing may then move work which has not started; active
// transfers on healthy replacements retain their assignments.
//
// A node that cannot rejoin is retried on the next step. For a node that
// joins after a node change, the reason is recorded (noteJoinProblem), so
// that a join that keeps failing or waiting is visible in the node view.
func (t *taskRuntime) rejoinAvailableNodes(ctx context.Context, p task.Parent) (task.Parent, error) {
	if p.Failover == nil || !p.Failover.Enabled {
		return p, nil
	}
	// Joins finished or cancelled since the last step keep no problem.
	t.joins.retain(p)
	switch {
	case len(p.Failover.Migrations) > 0:
		t.noteJoinsWaiting(p, newJoinProblem(joinTakeoverInProgress))
		return p, nil
	case p.Failover.ScannerSeeding:
		t.noteJoinsWaiting(p, newJoinProblem(joinScanHandoff))
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
	var displays *nodeDisplaySet
	for _, node := range p.Spec.Nodes {
		if !p.NodeRetired(node.ID) || p.NodeRemoved(node.ID) {
			continue
		}
		if ctx.Err() != nil {
			// Every remaining call would fail with the step's deadline, which
			// says nothing about the nodes.
			return p, nil
		}
		if displays == nil {
			snapshot := t.server.nodeDisplaySnapshot()
			displays = &snapshot
		}
		label := displays.lookup(node.ID, node.Address).DisplayLabel
		attempt := p
		var problem *joinProblem
		p, problem, err = t.rejoinNode(ctx, p, node, label, members)
		if problem != nil && ctx.Err() == nil {
			// The snapshot in which the node was found joining identifies its
			// join attempt. A returning node keeps no record.
			t.noteJoinProblem(attempt, node, label, *problem)
		}
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// rejoinNode runs the rejoin handshake of one retired or joining member. It
// returns why the node did not rejoin, and an error only when the step must
// end: the task cannot be read, or the step's deadline has passed.
func (t *taskRuntime) rejoinNode(ctx context.Context, p task.Parent, node task.Node, label string, members map[string]string) (task.Parent, *joinProblem, error) {
	wait := func(code string, args ...any) (task.Parent, *joinProblem, error) {
		problem := newJoinProblem(code, args...)
		return p, &problem, nil
	}
	enrollment, online := members[node.ID]
	switch {
	case !online:
		return wait(joinAgentOffline)
	case enrollment != node.EnrollmentID:
		// The node view also reports it offline: it is not online with the
		// enrollment that the task recorded.
		return wait(joinEnrollmentChanged)
	case !p.CanRejoinScanner(node.ID):
		return wait(joinScanUnfinished)
	}
	maintenance, err := t.server.agentUnderMaintenance(node.ID)
	if err != nil {
		return wait(joinMaintenanceUnknown, err.Error())
	}
	if maintenance {
		return wait(joinMaintenance)
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var caps memberCapabilities
	if err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps); err != nil {
		return wait(joinVersionUnavailable, err.Error())
	}
	if caps.LeaseProtocol < 2 {
		return wait(joinAgentTooOld)
	}
	local := p.Spec.Source.Kind == "LOCAL" || p.Spec.Destination.Kind == "LOCAL"
	unchecked := local && p.SharedCheckSkippedFor(node.ID)
	if unchecked && !sharedSkipCapable(caps) {
		// verifyTaskStorage would grant it a proof without a check, which
		// such an Agent cannot record.
		return wait(joinUpgradeRequired)
	}
	// A returning node can fail again during this handshake. Keep the healthy
	// scanner and transfer owners moving while durable intent is retried.
	failed := func(code string, err error) (task.Parent, *joinProblem, error) {
		current, readErr := t.repo.Get(p.ID)
		if readErr != nil {
			return p, nil, readErr
		}
		problem := newJoinProblem(code, err.Error())
		if code == joinNotRecorded && current.RejoinExhausted(node.ID) {
			problem = newJoinProblem(joinExhausted)
		}
		return current, &problem, ctx.Err()
	}
	if err := t.verifyTaskStorage(callCtx, p, []task.Node{node}, false); err != nil {
		return failed(joinStorageUnverified, err)
	}
	next, epoch, err := t.repo.BeginNodeRejoin(p.ID, p.Generation, node.ID)
	if err != nil {
		return failed(joinNotRecorded, err)
	}
	p = next
	if err := t.leases.rejoin(callCtx, p, node, epoch); err != nil {
		return failed(joinLeaseFailed, err)
	}
	if next, err = t.repo.FinishNodeRejoin(p.ID, p.Generation, node.ID, epoch); err != nil {
		return failed(joinNotRecorded, err)
	}
	p = next
	t.joinedNode(p, label, node.ID)
	if unchecked {
		log.Printf("task %d generation %d: node %s rejoins without a shared directory check, which the operator chose to skip", p.ID, p.Generation, label)
	}
	return p, nil, nil
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
