package master

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Dynamic task shares (see task.ShareState). The repository decides every
// share; the runtime only learns which node applied which revision, debounces
// idleness, commits allocation passes and delivers their result.

// A node must be idle in this many consecutive steps before its share is
// released. Becoming busy takes effect in the next pass.
const shareIdlePasses = 2

// Each step runs at most this many allocation passes: the first commits
// reductions (and raises within existing slack), the second folds the
// reductions just acknowledged and raises.
const shareRoundsPerStep = 2

// shareAck is the latest revision a node applied: its bandwidth_parent call
// returned success.
type shareAck struct {
	generation, revision uint64
}

type shareRuntime struct {
	acks    map[int64]map[string]shareAck
	idle    map[int64]map[string]int
	commits map[int64]uint64
	total   uint64
}

func staleLimit(err error) bool {
	if errors.Is(err, ratelimit.ErrStale) {
		return true
	}
	var remote *AgentError
	return errors.As(err, &remote) && remote.Message == ratelimit.ErrStale.Error()
}

// noteShareAck records that a node applied policy. Only a successful
// bandwidth_parent call is an acknowledgement; a lost reply leaves the node
// unacknowledged, which only delays its reserve from being released.
func (t *taskRuntime) noteShareAck(policy ratelimit.ParentLimit, nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &t.shares
	if s.acks == nil {
		s.acks = map[int64]map[string]shareAck{}
	}
	if s.acks[policy.ParentID] == nil {
		s.acks[policy.ParentID] = map[string]shareAck{}
	}
	last := s.acks[policy.ParentID][nodeID]
	if policy.Generation > last.generation || policy.Generation == last.generation && policy.Revision > last.revision {
		s.acks[policy.ParentID][nodeID] = shareAck{generation: policy.Generation, revision: policy.Revision}
	}
}

// shareAcked reports whether a node applied p's current revision.
func (t *taskRuntime) shareAcked(p task.Parent, nodeID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ack, ok := t.shares.acks[p.ID][nodeID]
	return ok && ack.generation == p.Generation && ack.revision == task.WireRevision(p)
}

func (t *taskRuntime) shareAckedNodes(p task.Parent) []string {
	var acked []string
	for _, node := range p.ActiveNodes() {
		if t.shareAcked(p, node.ID) {
			acked = append(acked, node.ID)
		}
	}
	return acked
}

// idleAfterPass returns the nodes idle for shareIdlePasses steps in a row;
// a busy node's count restarts at once. A step counts one more idle step
// (count); an extra pass within the same step does not. The counts are in
// memory: a restarted Master counts again, which keeps shares longer, never
// shorter.
func (t *taskRuntime) idleAfterPass(p task.Parent, count bool) map[string]bool {
	busy := task.ShareBusy(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &t.shares
	if s.idle == nil {
		s.idle = map[int64]map[string]int{}
	}
	counts := map[string]int{}
	idle := map[string]bool{}
	for _, node := range p.ActiveNodes() {
		if !busy[node.ID] {
			counts[node.ID] = s.idle[p.ID][node.ID]
			if count {
				counts[node.ID] = min(counts[node.ID]+1, shareIdlePasses)
			}
		}
		idle[node.ID] = counts[node.ID] >= shareIdlePasses
	}
	s.idle[p.ID] = counts
	return idle
}

// releasedNowBusy reports a node whose share was released as idle but which
// has work again (for example, a dispatch just placed a child on it).
func (t *taskRuntime) releasedNowBusy(p task.Parent) bool {
	busy := task.ShareBusy(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, count := range t.shares.idle[p.ID] {
		if count >= shareIdlePasses && busy[id] {
			return true
		}
	}
	return false
}

func (t *taskRuntime) shareCommits(parentID int64) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.shares.commits[parentID]
}

func (t *taskRuntime) forgetShares(parentID int64) {
	delete(t.shares.acks, parentID)
	delete(t.shares.idle, parentID)
	delete(t.shares.commits, parentID)
}

// deliverPolicy sends p's policy to its index-th active node and records the
// attempt and, on success, the acknowledgement.
func (t *taskRuntime) deliverPolicy(ctx context.Context, p task.Parent, index int, node task.Node) error {
	policy := parentPolicy(p, index)
	err := t.server.agentCall(ctx, nodeSession(node), "bandwidth_parent", true, policy, nil)
	t.notePolicyDelivery(p, node.ID, err)
	if err == nil {
		t.noteShareAck(policy, node.ID)
	}
	return err
}

// redeliverStale answers an Agent that rejected p's revision as stale. Another
// path (a lease renewal, a child recovery, an allocation pass) may already
// have delivered a newer revision: deliver the current one instead of treating
// a healthy node as unavailable. An older Master may also have delivered its
// own equal split at a higher revision without saving the parent; the revision
// then moves past it.
func (t *taskRuntime) redeliverStale(ctx context.Context, p task.Parent, node task.Node, stale error) error {
	current, err := t.repo.Get(p.ID)
	if err != nil || current.Generation != p.Generation {
		return stale
	}
	if task.WireRevision(current) == task.WireRevision(p) && current.Shares != nil {
		var held ratelimit.ParentLimit
		if t.server.agentCall(ctx, nodeSession(node), "bandwidth_parent_status", false, map[string]any{"parentId": p.ID}, &held) == nil &&
			held.Generation == current.Generation && held.Revision >= current.Shares.Revision {
			if skipped, err := t.repo.SkipShareRevisions(p.ID, p.Generation, current.Shares.Revision, held.Revision); err == nil {
				current = skipped
			}
		}
	}
	if task.WireRevision(current) <= task.WireRevision(p) {
		return stale
	}
	for i, member := range current.ActiveNodes() {
		if member.ID == node.ID {
			return t.deliverPolicy(ctx, current, i, member)
		}
	}
	return stale
}

// reallocateShares runs the step's allocation passes and delivers each
// committed change to the available nodes. Offline nodes are not waited for:
// they keep their reserve until they acknowledge or are fenced. A failed
// delivery leaves a node on its older value, which its reserve still covers;
// the next step's preparation retries it.
func (t *taskRuntime) reallocateShares(ctx context.Context, p task.Parent, available map[string]bool) task.Parent {
	return t.reallocate(ctx, p, available, true)
}

// reallocateWoken is an extra pass within a step, after work was placed on a
// node whose share had been released. It does not count as an idle step.
func (t *taskRuntime) reallocateWoken(ctx context.Context, p task.Parent, available map[string]bool) task.Parent {
	return t.reallocate(ctx, p, available, false)
}

func (t *taskRuntime) reallocate(ctx context.Context, p task.Parent, available map[string]bool, step bool) task.Parent {
	if p.Status != "RUNNING" || p.ResumeAfter != nil {
		return p
	}
	idle := t.idleAfterPass(p, step)
	for range shareRoundsPerStep {
		if ctx.Err() != nil {
			return p
		}
		acked := t.shareAckedNodes(p)
		if !task.ReallocationPending(p, acked, idle) {
			return p
		}
		next, changed, err := t.repo.ReallocateShares(p.ID, p.Generation, task.WireRevision(p), acked, idle)
		if err != nil || !changed {
			return p
		}
		p = next
		t.mu.Lock()
		if t.shares.commits == nil {
			t.shares.commits = map[int64]uint64{}
		}
		t.shares.commits[p.ID]++
		t.shares.total++
		t.mu.Unlock()
		t.deliverShares(ctx, p, available)
	}
	return p
}

// deliverShares sends p's policies to the available members, at most eight
// at once and each within five seconds.
func (t *taskRuntime) deliverShares(ctx context.Context, p task.Parent, available map[string]bool) {
	var workers sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, node := range p.ActiveNodes() {
		if !available[node.ID] {
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := t.deliverPolicy(callCtx, p, i, node); staleLimit(err) {
				_ = t.redeliverStale(callCtx, p, node, err)
			}
		}()
	}
	workers.Wait()
}

// shareView describes how a node's share stands for the node view and the
// bandwidth status: its demand, and whether it holds more than its share
// until it acknowledges a reduction.
func shareView(p task.Parent, busy map[string]bool, nodeID string) map[string]any {
	view := map[string]any{"demand": "busy", "adjusting": false}
	if p.Shares == nil {
		return view
	}
	if !busy[nodeID] {
		view["demand"] = "idle"
	}
	entry, ok := p.Shares.Nodes[nodeID]
	if !ok {
		return view
	}
	totals := task.ShareTotals(p.Spec)
	adjusting := totals.Bandwidth > 0 && entry.Reserved.Bandwidth > entry.Share.Bandwidth ||
		totals.List > 0 && entry.Reserved.List > entry.Share.List ||
		totals.Head > 0 && entry.Reserved.Head > entry.Share.Head ||
		totals.Put > 0 && entry.Reserved.Put > entry.Share.Put
	view["adjusting"] = adjusting
	view["reserved"] = map[string]any{"bandwidthBytesPerSecond": entry.Reserved.Bandwidth, "listQps": entry.Reserved.List, "headQps": entry.Reserved.Head, "putQps": entry.Reserved.Put}
	return view
}

// shareAllocation names how a task divides its limits.
func shareAllocation(p task.Parent) string {
	if p.Shares == nil {
		return "equal"
	}
	return "dynamic"
}
