package master

import (
	"context"
	"fmt"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func (t *taskRuntime) rebalanceQueued(ctx context.Context, p task.Parent, available map[string]bool) error {
	if !p.NeedsQueuedRebalance() || p.FailoverPending() {
		return nil
	}
	var err error
	p, err = t.repo.EnsureQueuedRebalance(p.ID, p.Generation)
	if err != nil || p.Failover.Rebalance == nil {
		return err
	}
	eligible := map[string]bool{}
	for _, node := range p.ActiveNodes() {
		if !available[node.ID] {
			continue
		}
		maint, err := t.server.agentUnderMaintenance(node.ID)
		if err != nil || maint {
			continue
		}
		var caps struct {
			QueuedHandoffProtocol int `json:"queuedHandoffProtocol"`
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
		cancel()
		if err == nil && caps.QueuedHandoffProtocol >= 1 {
			eligible[node.ID] = true
		}
	}
	// During rolling upgrades, keep the one-shot plan until every live owner
	// understands queue revocation. Otherwise old peers could consume both
	// passes before their new binary is installed.
	if len(eligible) != len(p.ActiveNodes()) {
		return nil
	}
	for range 8 {
		intent, err := t.repo.NextQueuedHandoff(p.ID, p.Generation, eligible)
		if err != nil || intent == nil {
			return err
		}
		if !eligible[intent.Source.ID] {
			return nil
		}
		request := execution.QueueReleaseRequest{ParentID: p.ID, ChildID: fmt.Sprintf("%d-%d", p.ID, intent.Number), Generation: intent.Generation, Assignment: intent.Assignment, OwnerEpoch: intent.OwnerEpoch}
		var ack struct {
			Released bool `json:"released"`
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = t.server.agentCall(callCtx, nodeSession(intent.Source), "child_release_queued", true, request, &ack)
		cancel()
		if err != nil {
			return err
		}
		moved, err := t.repo.FinishQueuedHandoff(p.ID, *intent, ack.Released, eligible)
		if err != nil {
			return err
		}
		if moved != nil && p.Status == "RUNNING" {
			// Ownership is already durable. A lost target ACK is handled by the
			// ordinary outstanding queue, never by reviving the old assignment.
			if err := t.deliverChildren(ctx, p, []task.Child{*moved}); err != nil {
				return err
			}
		}
	}
	return nil
}
