package master

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Execution-node changes reuse the takeover machinery:
//   - a joining node is a retired member that the rejoin handshake admits under
//     a fresh ownership epoch; existing members acknowledge their reduced
//     bandwidth/QPS share before its first lease grant (taskLeases.renew);
//   - a leaving node is fenced first (explicit lease revocation, or lease
//     expiry when offline), then BeginFailover moves its unfinished children
//     and, if needed, the scan. Only then are the other shares raised.

type memberCapabilities struct {
	LeaseProtocol          int `json:"leaseProtocol"`
	QueuedHandoffProtocol  int `json:"queuedHandoffProtocol"`
	ConflictPolicyProtocol int `json:"conflictPolicyProtocol"`
}

func (t *taskRuntime) capabilities(ctx context.Context, node task.Node) (memberCapabilities, error) {
	var caps memberCapabilities
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
	return caps, err
}

// A joining member must support recovery fencing (rejoin epochs) and queued
// handoff, the same capabilities required to restore a returning node.
func joinCapable(caps memberCapabilities) bool {
	return caps.LeaseProtocol >= 2 && caps.QueuedHandoffProtocol >= 1
}

// conflictPolicyCapable reports whether an Agent accepts jobs with this
// conflict policy. SKIP and OVERWRITE are understood by every Agent.
func conflictPolicyCapable(policy string, caps memberCapabilities) bool {
	return policy != execution.ConflictOverwriteChanged || caps.ConflictPolicyProtocol >= agentservice.ConflictPolicyProtocol
}

// revoke fences a reachable member immediately. The Agent acknowledges only
// after its retirement is durable and every task I/O has stopped, so the
// Master need not wait for the lease to expire.
func (l *taskLeases) revoke(ctx context.Context, p task.Parent, node task.Node) error {
	e, err := l.entry(p, node)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.record.Retired && e.retireAck {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := l.call(callCtx, nodeSession(node), "lease_retire", true, agentservice.LeaseRetireRequest{ParentID: p.ID, OwnerEpoch: p.NodeEpoch(node.ID)}, nil); err != nil {
		return err
	}
	old := e.record
	e.record.Retired = true
	if err := l.save(e); err != nil {
		e.record = old
		return err
	}
	e.retireAck = true
	wakeLease(e)
	return nil
}

func (t *taskRuntime) reconcileMembership(ctx context.Context, p task.Parent, available map[string]bool) (task.Parent, error) {
	if p.Membership == nil || len(p.Membership.Draining) == 0 || p.Failover == nil || !p.Failover.Enabled ||
		p.Status != "RUNNING" && p.Status != "RESUMING" || p.ResumeAfter != nil || p.FailoverPending() {
		return p, nil
	}
	ids := make([]string, 0, len(p.Membership.Draining))
	for id := range p.Membership.Draining {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var problems []error
	for _, id := range ids {
		drain, draining := p.NodeDraining(id)
		if !draining || !available[id] {
			// Offline members leave through the ordinary lease-expiry takeover.
			continue
		}
		var node task.Node
		for _, member := range p.Spec.Nodes {
			if member.ID == id {
				node = member
			}
		}
		target := false
		for _, candidate := range p.PlacementNodes() {
			target = target || available[candidate.ID]
		}
		if !target {
			continue // Wait for a joining node before giving up work.
		}
		if drain.Mode == task.DrainModeDrain {
			ready, err := t.repo.DrainReady(p.ID, id)
			if err != nil {
				return p, err
			}
			if !ready {
				continue
			}
		}
		next, err := t.leaveTask(ctx, p, node, drain.Mode == task.DrainModeNow, available)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		p = next
	}
	return p, errors.Join(problems...)
}

func (t *taskRuntime) leaveTask(ctx context.Context, p task.Parent, node task.Node, now bool, available map[string]bool) (task.Parent, error) {
	if err := t.leases.revoke(ctx, p, node); err != nil {
		name := t.server.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
		return p, fmt.Errorf("Agent %s could not be fenced for removal: %w", name, err)
	}
	// From here on the node refuses this task's work. The rest of this step
	// must not address it as an available member: its "retired" replies are
	// expected and would otherwise surface as a task error.
	delete(available, node.ID)
	if now {
		// Collect the fenced node's latest durable progress so completed
		// objects are not repeated by the replacement owner.
		var mine []task.Child
		after := 0
		for {
			page, err := t.repo.Outstanding(p.ID, after, 256)
			if err != nil {
				return p, err
			}
			for _, child := range page {
				after = child.Job.Number
				if child.Node.ID == node.ID {
					mine = append(mine, child)
				}
			}
			if len(page) < 256 {
				break
			}
		}
		if len(mine) > 0 {
			observed, _ := t.observeNode(ctx, p, mine, false)
			if err := t.repo.ObserveBatch(p.ID, p.Generation, observed); err != nil {
				return p, err
			}
		}
	}
	p, err := t.repo.BeginFailover(p.ID, p.Generation, node.ID)
	if err != nil {
		return p, err
	}
	targets := map[string]bool{}
	for _, candidate := range p.PlacementNodes() {
		if available[candidate.ID] {
			targets[candidate.ID] = true
		}
	}
	for range 64 {
		_, ready, err := t.repo.PrepareFailover(p.ID, p.Generation, node.ID, targets)
		if err != nil {
			return p, err
		}
		if ready {
			break
		}
	}
	if p, err = t.repo.Get(p.ID); err != nil {
		return p, err
	}
	return t.moveRetiredScanner(p, targets)
}

// membershipNodes describes current members for task rows and the node view.
func (s *Server) membershipNodes(p task.Parent, displays nodeDisplaySet, online map[string]string) []map[string]any {
	shares := map[string]map[string]any{}
	allocation := map[string]map[string]any{}
	busy := task.ShareBusy(p)
	for i, node := range p.ActiveNodes() {
		policy := parentPolicy(p, i)
		shares[node.ID] = map[string]any{"bandwidthBytesPerSecond": max(policy.BandwidthBytesPerSecond, 0), "listQps": policy.ListQPS, "headQps": policy.HeadQPS, "putQps": policy.PutQPS}
		allocation[node.ID] = shareView(p, busy, node.ID)
	}
	pending := p.PendingChildren()
	local := p.Spec.Source.Kind == "LOCAL" || p.Spec.Destination.Kind == "LOCAL"
	// Only a member that scans now, or takes the scan over from the Master's
	// checkpoint, is the scanner. A finished scan needs none, and a fenced
	// scanner waiting for its replacement is not a member that scans.
	scanner := ""
	if phase := scanPhase(p); phase == scanPhaseScanning || phase == scanPhaseHandoff {
		scanner = p.ScannerNode().ID
	}
	// Only lease fencing lets an offline node's work move without it.
	fenced := p.Failover != nil && p.Failover.Enabled && p.TakeoverAllowed()
	nodes := []map[string]any{}
	for _, node := range p.CurrentNodes() {
		state, mode := "ACTIVE", ""
		drain, draining := p.NodeDraining(node.ID)
		switch {
		case p.NodeJoining(node.ID):
			state = "JOINING"
		case draining && drain.Mode == task.DrainModeNow:
			state, mode = "REMOVING", "now"
		case draining:
			state, mode = "DRAINING", "drain"
		case p.NodeRetired(node.ID):
			state = "DISCONNECTED"
		}
		row := map[string]any{"id": node.ID, "address": node.Address, "state": state, "drainMode": mode, "scanner": scanner != "" && scanner == node.ID, "share": nil}
		if share, ok := shares[node.ID]; ok {
			row["share"] = share
			// demand: busy or idle; adjusting: the node still holds more than
			// its share until it acknowledges a reduction.
			for key, value := range allocation[node.ID] {
				row[key] = value
			}
		}
		if pending != nil {
			row["pendingChildren"] = pending[node.ID]
		}
		if online != nil {
			row["online"] = online[node.ID] == node.EnrollmentID
			if draining && !row["online"].(bool) {
				if fenced {
					// A leaving node that is offline leaves once its execution
					// lease expires; its work then moves to the remaining nodes.
					row["progress"] = "LEASE_EXPIRY"
				} else {
					// Without lease fencing nothing can take its work over: it
					// leaves after it reconnects and finishes or hands off its work.
					row["progress"] = "RECONNECT"
				}
			}
		}
		if local {
			row["localPath"] = node.LocalRoot
		}
		addNodeDisplay(row, displays.lookup(node.ID, node.Address))
		nodes = append(nodes, row)
	}
	return nodes
}

func (s *Server) membershipSummary(p task.Parent, displays nodeDisplaySet) map[string]any {
	joining, draining := []string{}, []string{}
	for _, node := range p.CurrentNodes() {
		if p.NodeJoining(node.ID) {
			joining = append(joining, node.Address)
		} else if _, ok := p.NodeDraining(node.ID); ok {
			draining = append(draining, node.Address)
		}
	}
	return map[string]any{"nodeRevision": p.NodeRevision(), "pending": p.MembershipPending(), "nodes": s.membershipNodes(p, displays, nil), "joining": joining, "draining": draining, "scanPhase": scanPhase(p)}
}

func membershipReason(ctx context.Context, err error) string {
	var rejection *task.MembershipError
	if errors.As(err, &rejection) {
		return fmt.Sprintf(localizeContext(ctx, rejection.Format), rejection.Args...)
	}
	if err != nil {
		return localizeContext(ctx, err.Error())
	}
	return ""
}

// taskNodesView reads Agent capabilities for candidates within one deadline.
func (s *Server) taskNodesView(ctx context.Context, p task.Parent) (map[string]any, error) {
	members, err := s.onlineAgents()
	if err != nil {
		return nil, err
	}
	online := map[string]string{}
	for _, member := range members {
		online[member.ID] = member.EnrollmentID
	}
	displays := s.nodeDisplaySnapshot()
	view := map[string]any{"taskId": p.ID, "generation": p.Generation, "nodeRevision": p.NodeRevision(), "status": p.Status, "direction": p.Spec.Direction,
		"localTransfer": p.Spec.Source.Kind == "LOCAL" || p.Spec.Destination.Kind == "LOCAL", "pending": p.MembershipPending(),
		"editable": true, "reason": "", "nowSupported": p.TakeoverAllowed(), "nowReason": "", "bandwidthLimitMbps": nil}
	if blocked := p.MembershipBlocked(); blocked != nil {
		view["editable"], view["reason"] = false, membershipReason(ctx, blocked)
	}
	if !p.TakeoverAllowed() {
		view["nowReason"] = localizeContext(ctx, "Immediate removal needs takeover support: OSS-to-OSS or a verified shared directory")
	}
	if p.Spec.BandwidthMbps > 0 {
		view["bandwidthLimitMbps"] = p.Spec.BandwidthMbps
	}
	view["allocation"] = shareAllocation(p)
	view["nodes"] = s.membershipNodes(p, displays, online)
	view["scan"] = map[string]any{"phase": scanPhase(p), "complete": scanFinished(p)}
	removed := []map[string]any{}
	current := map[string]bool{}
	for _, node := range p.CurrentNodes() {
		current[node.ID] = true
	}
	for _, node := range p.Spec.Nodes {
		if p.NodeRemoved(node.ID) {
			row := map[string]any{"id": node.ID, "address": node.Address, "removedAt": p.Membership.Removed[node.ID].UnixMilli()}
			addNodeDisplay(row, displays.lookup(node.ID, node.Address))
			removed = append(removed, row)
		}
	}
	view["removedNodes"] = removed
	candidates := make([]map[string]any, 0, len(members))
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	type result struct {
		index int
		row   map[string]any
	}
	results := make(chan result, len(members))
	count := 0
	slots := make(chan struct{}, 8)
	for _, member := range members {
		if current[member.ID] {
			continue
		}
		index := count
		count++
		node := task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address}
		go func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			row := map[string]any{"id": node.ID, "address": node.Address, "eligible": true, "reason": ""}
			addNodeDisplay(row, displays.lookup(node.ID, node.Address))
			if s.softwareUpdateBlocksNode(node.ID) {
				row["eligible"], row["reason"] = false, localizeContext(ctx, "Agent update is in progress")
			} else if caps, err := s.tasks.capabilities(callCtx, node); err != nil || !joinCapable(caps) || !conflictPolicyCapable(p.Spec.ConflictPolicy, caps) {
				row["eligible"], row["reason"] = false, localizeContext(ctx, "Upgrade this Agent to join running tasks")
			}
			results <- result{index, row}
		}()
	}
	rows := make([]map[string]any, count)
	for range count {
		r := <-results
		rows[r.index] = r.row
	}
	candidates = append(candidates, rows...)
	view["candidates"] = candidates
	return view, nil
}

// normalizedMembers returns a stable identity for a request body.
func normalizedMembers(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func trimmedList(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || seen[value] {
			return nil, errors.New("invalid or duplicate execution node")
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

// removedScannerDone reports a removed scanner whose scan is complete and fully
// assigned. It never returns, and the task needs no scanner any more: the step
// only waits for transfers instead of reporting a disconnected scanner.
func removedScannerDone(p task.Parent) bool {
	scanner := p.ScannerNode().ID
	return scanner != "" && p.NodeRemoved(scanner) && p.Status == "RUNNING" && scanFinished(p)
}
