package task

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// A task's bandwidth and request limits are task totals. The Master divides
// each total among the active nodes by demand: a node with work in front of
// it is busy, every other node keeps a small floor, and the busy nodes share
// the rest. Shares are a pure function of the durable parent, changed only
// inside a repository transaction, so every sender delivers the same value for
// one (generation, revision) and an Agent never sees two values for it.
//
// Reserved is the ledger that keeps the sum of what Agents may enforce within
// the total at every moment. For each node and dimension it is an upper bound
// of every value that node has installed or may still install: a raise is
// committed only out of total − Σ Reserved, and a node's Reserved falls to its
// Share only after that node has acknowledged the current revision (an Agent
// rejects every older revision after that). A reduction and a raise may be in
// one commit; the raise can only spend slack that already exists, so the
// reduction always takes effect first.

// ShareRates holds one node's rate in every task-scoped dimension. Zero means
// unlimited in that dimension.
type ShareRates struct {
	Bandwidth int64   `json:"bandwidth"`
	List      float64 `json:"list,omitempty"`
	Head      float64 `json:"head,omitempty"`
	Put       float64 `json:"put,omitempty"`
}

// NodeShare is one active node's delivered share and its ledger bound.
type NodeShare struct {
	Share    ShareRates `json:"share"`
	Reserved ShareRates `json:"reserved"`
}

// ShareState is the per-node allocation of a task's limits. Revision is the
// revision Agents receive; it is always BudgetRevision − 1, so an older Master
// (which drops this field and sends BudgetRevision) always sends a revision an
// Agent accepts instead of a conflicting value for a revision already used.
type ShareState struct {
	Revision uint64               `json:"revision"`
	Nodes    map[string]NodeShare `json:"nodes"`
}

const shareDimensions = 4 // bandwidth, LIST, HEAD, PUT

// The idle floor is 1/64 of an equal share: a node that has just become busy
// still progresses while its raise waits, and all idle floors together use at
// most 1/64 of the total.
const idleFloorDivisor = 64

// Smallest positive QPS an Agent accepts (ratelimit.validParent).
const minimumQPS = 1e-12

func (r ShareRates) value(d int) float64 {
	switch d {
	case 0:
		return float64(r.Bandwidth)
	case 1:
		return r.List
	case 2:
		return r.Head
	}
	return r.Put
}

func (r *ShareRates) setValue(d int, v float64) {
	switch d {
	case 0:
		r.Bandwidth = int64(v)
	case 1:
		r.List = v
	case 2:
		r.Head = v
	default:
		r.Put = v
	}
}

// shareLimits are a spec's limits per dimension: total is the cluster-wide
// amount to divide (0: none), cap a per-node ceiling (0: none).
type shareLimits struct {
	total, cap [shareDimensions]float64
}

// Bandwidth is an integer number of bytes per second; every value stays far
// below 2^53, so float64 holds it exactly.
func integerDimension(d int) bool { return d == 0 }

func limitsOf(spec Spec) shareLimits {
	var l shareLimits
	if spec.BandwidthMbps > 0 {
		l.total[0] = math.Floor(spec.BandwidthMbps * 125000)
	}
	if r := spec.RequestLimits; r != nil {
		perNode := r.Scope == "PER_NODE"
		if r.BandwidthBitsPerSecond > 0 {
			legacy := math.Floor(r.BandwidthBitsPerSecond / 8)
			if perNode {
				l.cap[0] = legacy
			} else if l.total[0] == 0 || legacy < l.total[0] {
				l.total[0] = legacy
			}
		}
		for i, v := range [3]float64{r.ListQPS, r.HeadQPS, r.PutQPS} {
			if perNode {
				l.cap[i+1] = v
			} else {
				l.total[i+1] = v
			}
		}
	}
	return l
}

// ShareTotals returns the task totals that are divided among nodes; zero
// dimensions are not divided (unlimited, or a per-node cap only).
func ShareTotals(spec Spec) ShareRates {
	l := limitsOf(spec)
	var out ShareRates
	for d := range shareDimensions {
		out.setValue(d, l.total[d])
	}
	return out
}

// WireRevision is the revision Agents receive for p's current limits.
func WireRevision(p Parent) uint64 {
	if p.Shares != nil {
		return p.Shares.Revision
	}
	return p.BudgetRevision
}

func budgetExhausted(p Parent) bool { return p.BudgetRevision > math.MaxUint64-2 }

// bumpBudget advances the revision for a changed policy. With shares it moves
// by two, keeping the wire revision one below BudgetRevision (see ShareState).
func bumpBudget(p *Parent) error {
	if budgetExhausted(*p) {
		return ErrConflict
	}
	if p.Shares == nil {
		p.BudgetRevision++
		return nil
	}
	p.BudgetRevision += 2
	p.Shares.Revision = p.BudgetRevision - 1
	return nil
}

// NodePolicy is the limit delivered to the index-th active node. All senders
// use it, so a (generation, revision) pair always carries one value per node.
func (p Parent) NodePolicy(index int) ratelimit.ParentLimit {
	active := p.ActiveNodes()
	policy := ratelimit.ParentLimit{ParentID: p.ID, Generation: p.Generation, Revision: WireRevision(p), Stopped: p.Status == "STOPPING" || p.Status == "STOPPED"}
	if p.Shares == nil {
		return legacyNodePolicy(p, active, index, policy)
	}
	if index < 0 || index >= len(active) {
		return policy
	}
	share := p.Shares.Nodes[active[index].ID].Share
	policy.BandwidthBytesPerSecond = share.Bandwidth
	policy.ListQPS, policy.HeadQPS, policy.PutQPS = share.List, share.Head, share.Put
	return policy
}

// NodePolicyFor is NodePolicy by node ID; ok is false for an inactive node.
func (p Parent) NodePolicyFor(nodeID string) (ratelimit.ParentLimit, bool) {
	for i, node := range p.ActiveNodes() {
		if node.ID == nodeID {
			return p.NodePolicy(i), true
		}
	}
	return ratelimit.ParentLimit{}, false
}

// legacyNodePolicy is the fixed equal split of releases before dynamic shares.
// Tasks keep it until every member acknowledged it (see ReallocateShares).
func legacyNodePolicy(p Parent, active []Node, index int, policy ratelimit.ParentLimit) ratelimit.ParentLimit {
	total := int64(math.Floor(p.Spec.BandwidthMbps * 125000))
	members := int64(len(active))
	if total > 0 && members > 0 {
		policy.BandwidthBytesPerSecond = total / members
		if int64(index) < total%members {
			policy.BandwidthBytesPerSecond++
		}
	}
	spec := p.Spec
	spec.Nodes = active
	return ApplyRequestLimits(policy, spec)
}

// ShareBusy reports, for every active node, whether it needs its share now.
// It reads only durable scheduling state. Outside a plain RUNNING state, while
// work moves after a failover, or before the scheduling index exists, every
// node counts as busy.
func ShareBusy(p Parent) map[string]bool {
	active := p.ActiveNodes()
	busy := make(map[string]bool, len(active))
	if p.Scheduling == nil || p.FailoverPending() || p.Status != "RUNNING" || p.ResumeAfter != nil {
		for _, node := range active {
			busy[node.ID] = true
		}
		return busy
	}
	// The nodes that the next dispatch can place work on (chooseNode).
	placement := map[string]bool{}
	if nodes := p.PlacementNodes(); len(nodes) > 0 {
		if p.Spec.DistributionMode == "BALANCED" {
			for _, node := range nodes {
				placement[node.ID] = true
			}
		} else {
			placement[nodes[0].ID] = true
		}
	}
	undispatched := !p.ScanComplete || p.DispatchedThrough < p.ScannedBatches
	queued := p.NeedsQueuedRebalance()
	var handoff *QueuedHandoff
	if p.Failover != nil && p.Failover.Rebalance != nil {
		handoff = p.Failover.Rebalance.Pending
	}
	scanner := p.ScannerNode().ID
	for _, node := range active {
		id := node.ID
		busy[id] = p.Scheduling.Nodes[id].Children > 0 ||
			handoff != nil && (handoff.Source.ID == id || handoff.Target.ID == id) ||
			placement[id] && (undispatched || queued) ||
			id == scanner && !p.ScanComplete // the scan needs LIST requests
	}
	return busy
}

// IdleNow is the set of active nodes that ShareBusy reports idle.
func IdleNow(p Parent) map[string]bool {
	idle := map[string]bool{}
	for id, busy := range ShareBusy(p) {
		if !busy {
			idle[id] = true
		}
	}
	return idle
}

// demand combines the caller's idle set with the parent's current state: a
// node is idle only if the caller found it idle and it still is. With no busy
// node at all every node counts as busy, so a target always exists.
func demand(p Parent, active []Node, idle map[string]bool) (busy []bool, any bool) {
	now := ShareBusy(p)
	busy = make([]bool, len(active))
	for i, node := range active {
		busy[i] = now[node.ID] || !idle[node.ID]
		any = any || busy[i]
	}
	if !any {
		for i := range busy {
			busy[i] = true
		}
	}
	return busy, any
}

func sum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

// idleFloor is an idle node's share of a total divided among n nodes.
func idleFloor(total, cap float64, n int, integer bool) float64 {
	floor := total / float64(idleFloorDivisor*n)
	if integer {
		floor = math.Max(1, math.Floor(floor))
	} else {
		floor = math.Max(minimumQPS, floor)
	}
	if cap > 0 {
		floor = math.Min(floor, cap)
	}
	return floor
}

// waterfill divides budget max-min fairly: no value exceeds its cap (0: none)
// and no uncapped value is below another's. Integer budgets are divided
// exactly; the remainder goes one unit each to the earliest entries.
func waterfill(budget float64, caps []float64, integer bool) []float64 {
	out := make([]float64, len(caps))
	open := make([]bool, len(caps))
	count := len(caps)
	for i := range open {
		open[i] = true
	}
	remaining := math.Max(0, budget)
	for count > 0 {
		level := remaining / float64(count)
		if integer {
			level = math.Floor(level)
		}
		capped := false
		for i, c := range caps {
			if open[i] && c > 0 && c <= level {
				out[i], open[i] = c, false
				remaining -= c
				count--
				capped = true
			}
		}
		if capped {
			continue
		}
		extra := remaining - level*float64(count)
		for i := range caps {
			if !open[i] {
				continue
			}
			out[i] = level
			if integer && extra >= 1 && (caps[i] == 0 || caps[i] >= level+1) {
				out[i]++
				extra--
			}
		}
		break
	}
	return out
}

// fitWithin lowers eligible values, largest first and never below their
// floor, until their sum is at most limit. Float division can round a sum a
// few ulps above its total; integer values stay integers. The loop is bounded
// so an unexpected input can never spin.
func fitWithin(values []float64, limit float64, floorOf func(int) float64, integer bool) {
	for range 64 * (len(values) + 1) {
		excess := sum(values) - limit
		if excess <= 0 {
			return
		}
		largest := -1
		for i, v := range values {
			if v > floorOf(i) && (largest < 0 || v > values[largest]) {
				largest = i
			}
		}
		if largest < 0 {
			return
		}
		next := values[largest] - excess
		if integer {
			next = values[largest] - math.Ceil(excess)
		} else {
			next = math.Min(next, math.Nextafter(values[largest], 0))
		}
		values[largest] = math.Max(floorOf(largest), next)
	}
}

// targets returns each active node's target in dimension d.
func targets(l shareLimits, d int, busy []bool) []float64 {
	n := len(busy)
	out := make([]float64, n)
	total, cap, integer := l.total[d], l.cap[d], integerDimension(d)
	if total == 0 {
		for i := range out {
			out[i] = cap
		}
		return out
	}
	floor := idleFloor(total, cap, n, integer)
	budget := total
	var caps []float64
	var members []int
	for i, b := range busy {
		if b {
			caps = append(caps, cap)
			members = append(members, i)
		} else {
			out[i] = floor
			budget -= floor
		}
	}
	for j, v := range waterfill(budget, caps, integer) {
		out[members[j]] = v
	}
	if !integer {
		fitWithin(out, total, func(int) float64 { return 0 }, false)
	}
	return out
}

// allocateDimension moves one dimension toward target; see the package
// comment above for the ledger rules. acked nodes have applied the current
// revision.
func allocateDimension(share, reserved, target []float64, acked []bool, total float64, integer bool) {
	n := len(share)
	for i := range n {
		if acked[i] {
			reserved[i] = share[i]
		}
	}
	next := make([]float64, n)
	for i := range n {
		// Reduce now. Raising back up to Reserved is free: the reserves of the
		// nodes that may transfer always sum within the total (outside a limit
		// change, where they sum within the larger of the old and new totals).
		next[i] = math.Max(math.Min(target[i], share[i]), math.Min(target[i], reserved[i]))
	}
	before := append([]float64(nil), reserved...)
	if slack := total - sum(reserved); slack > 0 {
		var wants []float64
		var members []int
		for i := range n {
			// A positive want means next == reserved, so every granted unit
			// costs one unit of slack.
			if want := target[i] - next[i]; want > 0 {
				wants = append(wants, want)
				members = append(members, i)
			}
		}
		if len(wants) > 0 {
			for j, grant := range waterfill(slack, wants, integer) {
				next[members[j]] += grant
			}
		}
	}
	for i := range n {
		reserved[i] = math.Max(reserved[i], next[i])
	}
	if sum(reserved) > math.Max(sum(before), total) {
		// Only float rounding of granted slack gets here; take it back from
		// the raised nodes.
		fitWithin(reserved, total, func(i int) float64 { return before[i] }, integer)
		for i := range n {
			next[i] = math.Min(next[i], reserved[i])
		}
	}
	copy(share, next)
}

// allocateShares moves p.Shares toward the demand targets: it folds the
// acknowledged nodes' ledger, reduces, and raises within the slack. It reports
// whether any delivered share changed.
func allocateShares(p *Parent, acked map[string]bool, idle map[string]bool) bool {
	active := p.ActiveNodes()
	busy, _ := demand(*p, active, idle)
	l := limitsOf(p.Spec)
	changed := false
	for d := range shareDimensions {
		share := make([]float64, len(active))
		reserved := make([]float64, len(active))
		ack := make([]bool, len(active))
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			share[i], reserved[i], ack[i] = entry.Share.value(d), entry.Reserved.value(d), acked[node.ID]
		}
		before := append([]float64(nil), share...)
		if l.total[d] == 0 {
			for i := range share {
				share[i], reserved[i] = l.cap[d], l.cap[d]
			}
		} else {
			allocateDimension(share, reserved, targets(l, d, busy), ack, l.total[d], integerDimension(d))
		}
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			entry.Share.setValue(d, share[i])
			entry.Reserved.setValue(d, reserved[i])
			p.Shares.Nodes[node.ID] = entry
			changed = changed || share[i] != before[i]
		}
	}
	return changed
}

// seedShares starts a fresh allocation in which every node has applied its
// share: at creation, and for a new generation (whose start fences every older
// generation on the Agents).
func seedShares(p *Parent) {
	active := p.ActiveNodes()
	busy, _ := demand(*p, active, IdleNow(*p))
	l := limitsOf(p.Spec)
	revision := uint64(0)
	if p.Shares != nil {
		revision = p.Shares.Revision
	}
	p.Shares = &ShareState{Revision: revision, Nodes: make(map[string]NodeShare, len(active))}
	for d := range shareDimensions {
		values := targets(l, d, busy)
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			entry.Share.setValue(d, values[i])
			entry.Reserved.setValue(d, values[i])
			p.Shares.Nodes[node.ID] = entry
		}
	}
}

// materializeShares converts a task that still uses the fixed equal split. The
// caller has established that every active node acknowledged the current
// revision, so the equal split is exactly what each node may enforce.
func materializeShares(p *Parent) bool {
	active := p.ActiveNodes()
	if len(active) == 0 {
		return false
	}
	l := limitsOf(p.Spec)
	state := &ShareState{Nodes: make(map[string]NodeShare, len(active))}
	shares := make([][shareDimensions]float64, len(active))
	for i, node := range active {
		policy := p.NodePolicy(i)
		if policy.BandwidthBytesPerSecond < 0 {
			return false
		}
		values := [shareDimensions]float64{float64(policy.BandwidthBytesPerSecond), policy.ListQPS, policy.HeadQPS, policy.PutQPS}
		shares[i] = values
		var entry NodeShare
		for d := range shareDimensions {
			entry.Share.setValue(d, values[d])
			entry.Reserved.setValue(d, values[d])
		}
		state.Nodes[node.ID] = entry
	}
	for d := range shareDimensions {
		if l.total[d] == 0 {
			for _, node := range active {
				entry := state.Nodes[node.ID]
				if entry.Share.value(d) != l.cap[d] {
					return false
				}
			}
			continue
		}
		// The equal split of a float total can exceed it by a few ulps; the
		// delivered shares must not. Reserved keeps the installed values.
		column := make([]float64, len(active))
		for i := range active {
			column[i] = shares[i][d]
			if column[i] <= 0 {
				return false
			}
		}
		fitWithin(column, l.total[d], func(int) float64 { return 0 }, integerDimension(d))
		for i, node := range active {
			entry := state.Nodes[node.ID]
			entry.Share.setValue(d, column[i])
			state.Nodes[node.ID] = entry
		}
	}
	p.Shares = state
	return true
}

// rejoinShares admits nodeID into the allocation. Existing members are only
// reduced; the joiner receives its target and reserves it. The sum of Reserved
// may exceed the total until reductions are acknowledged; the joiner's lease
// gate (Master) waits until the effective sum fits.
func rejoinShares(p *Parent, nodeID string) {
	active := p.ActiveNodes()
	busy, _ := demand(*p, active, IdleNow(*p))
	l := limitsOf(p.Spec)
	for d := range shareDimensions {
		values := targets(l, d, busy)
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			switch {
			case node.ID == nodeID:
				entry.Share.setValue(d, values[i])
				entry.Reserved.setValue(d, values[i])
			case l.total[d] == 0:
				entry.Share.setValue(d, values[i])
				entry.Reserved.setValue(d, values[i])
			default:
				entry.Share.setValue(d, math.Min(values[i], entry.Share.value(d)))
			}
			p.Shares.Nodes[node.ID] = entry
		}
	}
}

// changeShareLimits re-divides after an edit of the task limits. A dimension
// that was already divided (or capped per node) only reduces now and raises
// within the new total's slack, so the enforced sum never exceeds the larger
// of the old and new totals. A dimension that was unlimited starts at its
// targets: until each Agent applies them it is unlimited anyway.
func changeShareLimits(p *Parent, old Spec) {
	active := p.ActiveNodes()
	busy, _ := demand(*p, active, IdleNow(*p))
	before, after := limitsOf(old), limitsOf(p.Spec)
	for d := range shareDimensions {
		share := make([]float64, len(active))
		reserved := make([]float64, len(active))
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			share[i], reserved[i] = entry.Share.value(d), entry.Reserved.value(d)
		}
		values := targets(after, d, busy)
		switch {
		case after.total[d] == 0 || before.total[d] == 0 && before.cap[d] == 0:
			copy(share, values)
			copy(reserved, values)
		default:
			// A per-node-only dimension stored each node's cap as both its
			// share and reserve, which is what each node enforces.
			allocateDimension(share, reserved, values, make([]bool, len(active)), after.total[d], integerDimension(d))
		}
		for i, node := range active {
			entry := p.Shares.Nodes[node.ID]
			entry.Share.setValue(d, share[i])
			entry.Reserved.setValue(d, reserved[i])
			p.Shares.Nodes[node.ID] = entry
		}
	}
}

// pruneShares drops entries of nodes that left when a completed task settled
// its membership; nothing is delivered for a finished task.
func pruneShares(p *Parent) {
	if p.Shares == nil {
		return
	}
	active := map[string]bool{}
	for _, node := range p.ActiveNodes() {
		active[node.ID] = true
	}
	for id := range p.Shares.Nodes {
		if !active[id] {
			delete(p.Shares.Nodes, id)
		}
	}
}

// validateShares checks the allocation invariants on every save.
func validateShares(p Parent) error {
	s := p.Shares
	if s.Revision == 0 || s.Revision != p.BudgetRevision-1 {
		return fmt.Errorf("share revision %d does not trail budget revision %d", s.Revision, p.BudgetRevision)
	}
	active := p.ActiveNodes()
	if len(s.Nodes) != len(active) {
		return errors.New("shares do not cover exactly the active nodes")
	}
	l := limitsOf(p.Spec)
	for d := range shareDimensions {
		total := 0.0
		for _, node := range active {
			entry, ok := s.Nodes[node.ID]
			if !ok {
				return fmt.Errorf("active node %s has no share", node.ID)
			}
			share, reserved := entry.Share.value(d), entry.Reserved.value(d)
			if l.total[d] == 0 {
				if share != l.cap[d] || reserved != l.cap[d] {
					return fmt.Errorf("undivided dimension %d of node %s is not its cap", d, node.ID)
				}
				continue
			}
			if math.IsNaN(share) || share <= 0 || share > reserved || l.cap[d] > 0 && share > l.cap[d] || integerDimension(d) && share != math.Floor(share) {
				return fmt.Errorf("invalid share in dimension %d of node %s", d, node.ID)
			}
			total += share
		}
		if l.total[d] > 0 && total > l.total[d] {
			return fmt.Errorf("shares of dimension %d exceed the task total", d)
		}
	}
	return nil
}

// checkShares runs before every save. Tests fail on any violation, so a path
// that changes the active nodes without the allocation is caught. A release
// binary instead falls back to the fixed equal split, which an Agent accepts
// (its revision, BudgetRevision, is above every share revision already sent);
// the allocation is rebuilt once every node has applied it.
func checkShares(p *Parent) error {
	if p.Shares == nil {
		return nil
	}
	err := validateShares(*p)
	if err == nil {
		return nil
	}
	if testing.Testing() {
		return err
	}
	p.Shares = nil
	return nil
}

// ReallocateShares commits one allocation pass for a running task. acked are
// the nodes that applied revision wire of this generation; idle are the nodes
// the caller has seen idle long enough (a node busy now stays busy). Nothing
// is written unless a delivered share changes, or the task is still on the
// equal split and every active node has applied it, which converts it.
func (r *Repository) ReallocateShares(id int64, generation, wire uint64, acked []string, idle map[string]bool) (p Parent, changed bool, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation || WireRevision(p) != wire || p.Status != "RUNNING" || p.ResumeAfter != nil || p.Deleted || p.Purged {
			return errUnchanged
		}
		ack := make(map[string]bool, len(acked))
		for _, nodeID := range acked {
			ack[nodeID] = true
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		if !reallocate(&p, ack, idle) {
			return errUnchanged
		}
		if err := bumpBudget(&p); err != nil {
			return err
		}
		changed = true
		return r.save(tx, p)
	}))
	return
}

// ReallocationPending reports, without writing, whether an allocation pass
// on this snapshot would commit. The runtime asks before opening a write
// transaction, so a settled task costs no database write lock per step. A
// missing scheduling index is rebuilt only inside the transaction, so such a
// snapshot always answers true.
func ReallocationPending(p Parent, acked []string, idle map[string]bool) bool {
	if p.Scheduling == nil {
		return true
	}
	if p.Shares != nil {
		shares := *p.Shares
		shares.Nodes = make(map[string]NodeShare, len(p.Shares.Nodes))
		for id, entry := range p.Shares.Nodes {
			shares.Nodes[id] = entry
		}
		p.Shares = &shares
	}
	ack := make(map[string]bool, len(acked))
	for _, nodeID := range acked {
		ack[nodeID] = true
	}
	return reallocate(&p, ack, idle)
}

// reallocate is one allocation pass; it reports whether p must be committed
// under a new revision.
func reallocate(p *Parent, acked, idle map[string]bool) bool {
	if p.Shares == nil {
		for _, node := range p.ActiveNodes() {
			if !acked[node.ID] {
				return false
			}
		}
		if !materializeShares(p) {
			return false
		}
		// The converted ledger holds exactly what the nodes enforce, so the
		// first allocation may already reduce and raise.
		allocateShares(p, nil, idle)
		return true
	}
	if _, any := demand(*p, p.ActiveNodes(), idle); !any {
		return false
	}
	return allocateShares(p, acked, idle)
}

// leaveShares releases a fenced node's reserve and lets the remaining nodes
// use it in the same commit.
func leaveShares(p *Parent, nodeID string) {
	if p.Shares == nil {
		return
	}
	delete(p.Shares.Nodes, nodeID)
	allocateShares(p, nil, IdleNow(*p))
}

// SharesFit reports whether the reserves of all active nodes fit every total.
// A joining node is granted its lease only then. The check must use the
// durable ledger, not acknowledgements known only in memory: a share may be
// raised up to its node's reserve without spending slack, so every reserve
// counted as released must be released durably (FoldShares).
func SharesFit(p Parent) bool {
	if p.Shares == nil {
		return false
	}
	l := limitsOf(p.Spec)
	for d := range shareDimensions {
		if l.total[d] == 0 {
			continue
		}
		total := 0.0
		for _, node := range p.ActiveNodes() {
			total += p.Shares.Nodes[node.ID].Reserved.value(d)
		}
		if total > l.total[d] {
			return false
		}
	}
	return true
}

// foldShares lowers the reserve of every acknowledged node to its share.
func foldShares(p *Parent, acked map[string]bool) bool {
	changed := false
	for _, node := range p.ActiveNodes() {
		entry, ok := p.Shares.Nodes[node.ID]
		if !ok || !acked[node.ID] || entry.Reserved == entry.Share {
			continue
		}
		entry.Reserved = entry.Share
		p.Shares.Nodes[node.ID] = entry
		changed = true
	}
	return changed
}

// FoldShares durably releases the reserves of nodes that applied revision
// wire of this generation. Delivered shares do not change, so neither does
// the revision.
func (r *Repository) FoldShares(id int64, generation, wire uint64, acked []string) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation || p.Shares == nil || p.Shares.Revision != wire {
			return errUnchanged
		}
		ack := make(map[string]bool, len(acked))
		for _, nodeID := range acked {
			ack[nodeID] = true
		}
		if !foldShares(&p, ack) {
			return errUnchanged
		}
		return r.save(tx, p)
	}))
	return
}

// SkipShareRevisions recovers from an older Master that sent its own equal
// split at BudgetRevision without saving the parent: the Agent then holds a
// revision at or above the share revision and rejects it as stale. Every
// active node may enforce that equal split, so it is reserved, and the
// revision moves past what the Agent holds.
func (r *Repository) SkipShareRevisions(id int64, generation, wire, held uint64) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation || p.Shares == nil || p.Shares.Revision != wire || held < wire || Ended(p.Status) || p.Deleted {
			return errUnchanged
		}
		if held > math.MaxUint64-4 {
			return ErrConflict
		}
		legacy := p
		legacy.Shares = nil
		legacy.BudgetRevision = held
		for i, node := range p.ActiveNodes() {
			policy := legacy.NodePolicy(i)
			entry := p.Shares.Nodes[node.ID]
			values := [shareDimensions]float64{float64(max(policy.BandwidthBytesPerSecond, 0)), policy.ListQPS, policy.HeadQPS, policy.PutQPS}
			for d := range shareDimensions {
				if ShareTotals(p.Spec).value(d) > 0 {
					entry.Reserved.setValue(d, math.Max(entry.Reserved.value(d), values[d]))
				}
			}
			p.Shares.Nodes[node.ID] = entry
		}
		p.BudgetRevision = max(p.BudgetRevision, held+1)
		if err := bumpBudget(&p); err != nil {
			return err
		}
		return r.save(tx, p)
	}))
	return
}
