package task

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// Removal modes. DRAIN lets running children finish on the node; NOW revokes
// its execution lease and continues the work through the takeover path.
const (
	DrainModeDrain = "DRAIN"
	DrainModeNow   = "NOW"
)

const nodeRequestBucket = "master-task-node-requests"

// MembershipState records execution-node changes made after creation. Joining
// nodes are members without authority until the rejoin handshake grants them a
// fresh ownership epoch. Draining nodes keep executing but receive no new
// placement. Removed nodes are fenced and never rejoin automatically.
type MembershipState struct {
	Revision uint64               `json:"revision"`
	Joining  map[string]time.Time `json:"joining,omitempty"`
	Draining map[string]NodeDrain `json:"draining,omitempty"`
	Removed  map[string]time.Time `json:"removed,omitempty"`
}

type NodeDrain struct {
	Mode        string    `json:"mode"`
	RequestedAt time.Time `json:"requestedAt"`
}

// MembershipError is a user-correctable rejection with a stable code.
// Format and Args are kept separately so the API can localize the template.
type MembershipError struct {
	Code   string
	Format string
	Args   []any
}

func (e *MembershipError) Error() string { return fmt.Sprintf(e.Format, e.Args...) }

func rejectMembership(code, format string, args ...any) error {
	return &MembershipError{Code: code, Format: format, Args: args}
}

func (p Parent) NodeJoining(id string) bool {
	if p.Membership == nil {
		return false
	}
	_, ok := p.Membership.Joining[id]
	return ok
}

func (p Parent) NodeDraining(id string) (NodeDrain, bool) {
	if p.Membership == nil {
		return NodeDrain{}, false
	}
	d, ok := p.Membership.Draining[id]
	return d, ok
}

func (p Parent) NodeRemoved(id string) bool {
	if p.Membership == nil {
		return false
	}
	_, ok := p.Membership.Removed[id]
	return ok
}

func (p Parent) NodeRevision() uint64 {
	if p.Membership == nil {
		return 0
	}
	return p.Membership.Revision
}

// MembershipPending reports a change that has not yet taken full effect.
func (p Parent) MembershipPending() bool {
	return p.Membership != nil && len(p.Membership.Joining)+len(p.Membership.Draining) > 0
}

// PlacementNodes are the active members that may receive new or moved work.
func (p Parent) PlacementNodes() []Node {
	active := p.ActiveNodes()
	nodes := active[:0:0]
	for _, node := range active {
		if _, draining := p.NodeDraining(node.ID); !draining {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// CurrentNodes lists every member that has not been removed, in creation order.
func (p Parent) CurrentNodes() []Node {
	nodes := make([]Node, 0, len(p.Spec.Nodes))
	for _, node := range p.Spec.Nodes {
		if !p.NodeRemoved(node.ID) {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// TakeoverAllowed reports whether a node's unfinished work may continue on
// another node: OSS-to-OSS or verified shared directories, never legacy tasks.
func (p Parent) TakeoverAllowed() bool {
	return p.Legacy == nil && (p.Spec.Source.Kind != "LOCAL" || p.Spec.SharedLocalSource)
}

// MembershipBlocked returns why execution nodes cannot be changed, or nil.
func (p Parent) MembershipBlocked() error {
	switch {
	case p.Deleted || p.Purged || p.Cleanup != nil:
		return rejectMembership("DELETED", "Deleted tasks cannot change execution nodes")
	case p.Status == "COMPLETED":
		return rejectMembership("COMPLETED", "Completed tasks cannot change execution nodes")
	case p.Legacy != nil:
		return rejectMembership("LEGACY", "Tasks migrated from the previous version cannot change execution nodes")
	case p.Spec.ObjectKey != "":
		return rejectMembership("SINGLE_OBJECT", "Single-object and single-file tasks cannot change execution nodes")
	case p.Status == "STOPPING" || p.Status == "RESUMING":
		return rejectMembership("TRANSITION", "Wait until the task has finished stopping or resuming")
	}
	return nil
}

// NodeChange is one validated membership request. Nodes are already resolved
// to enrolled identities (and local roots) by the caller.
type NodeChange struct {
	Add                []Node
	Remove             []string
	Mode               string
	RequestKey         string
	Fingerprint        string
	ExpectedGeneration *uint64
	ExpectedRevision   *uint64
}

type nodeRequestRecord struct {
	Fingerprint string `json:"fingerprint"`
	Revision    uint64 `json:"revision"`
}

func nodeRequestKey(id int64, requestKey string) string { return key(id) + "/" + digest(requestKey) }

// CheckNodeChange validates a request against the current parent without
// writing. The same check runs again inside ChangeNodes' transaction.
func (p Parent) CheckNodeChange(change NodeChange) error {
	if err := p.MembershipBlocked(); err != nil {
		return err
	}
	if change.ExpectedGeneration != nil && *change.ExpectedGeneration != p.Generation ||
		change.ExpectedRevision != nil && *change.ExpectedRevision != p.NodeRevision() {
		return rejectMembership("STALE", "The task's execution nodes changed; refresh and try again")
	}
	if change.Mode != DrainModeDrain && change.Mode != DrainModeNow {
		return errors.New("removal mode must be drain or now")
	}
	if len(change.Add)+len(change.Remove) == 0 || len(change.Add) > 64 || len(change.Remove) > 64 {
		return errors.New("add or remove between 1 and 64 execution nodes")
	}
	members := map[string]Node{}
	for _, node := range p.Spec.Nodes {
		members[node.ID] = node
	}
	seen := map[string]bool{}
	pending := p.MembershipPending()
	for _, node := range change.Add {
		if node.ID == "" || node.EnrollmentID == "" || seen[node.ID] {
			return errors.New("invalid or duplicate execution node")
		}
		seen[node.ID] = true
		if _, member := members[node.ID]; !member {
			if pending {
				return p.pendingRejection()
			}
			continue
		}
		_, draining := p.NodeDraining(node.ID)
		switch {
		case p.NodeRemoved(node.ID):
			if pending {
				return p.pendingRejection()
			}
		case draining:
			// Cancels the drain or the not yet fenced immediate removal; always
			// allowed, so a pending change can be undone.
		case p.NodeJoining(node.ID):
			return rejectMembership("ALREADY_MEMBER", "Agent %s is already joining this task", node.Address)
		case p.NodeRetired(node.ID):
			return rejectMembership("ALREADY_MEMBER", "Agent %s is already a member; it rejoins automatically after reconnecting", node.Address)
		default:
			return rejectMembership("ALREADY_MEMBER", "Agent %s is already an execution node of this task", node.Address)
		}
	}
	for _, id := range change.Remove {
		if seen[id] {
			return errors.New("the same execution node cannot be added and removed")
		}
		seen[id] = true
		node, member := members[id]
		if !member || p.NodeRemoved(id) {
			return rejectMembership("NOT_MEMBER", "Agent %s is not an execution node of this task", id)
		}
		drain, draining := p.NodeDraining(id)
		switch {
		case p.NodeJoining(id):
		case draining:
			if drain.Mode == DrainModeNow || change.Mode == DrainModeDrain {
				return rejectMembership("CHANGE_PENDING", "Agent %s is already being removed", node.Address)
			}
		case p.NodeRetired(id):
			// A disconnected peer whose work was taken over; removing it only
			// prevents its rejoin and is allowed while other changes are pending.
		default:
			if pending {
				return p.pendingRejection()
			}
		}
	}
	after := p.applyNodeChange(change, time.Time{})
	if change.Mode == DrainModeNow && len(change.Remove) > 0 && !after.TakeoverAllowed() {
		return rejectMembership("NOW_UNSUPPORTED", "Immediate removal needs takeover support: OSS-to-OSS or a verified shared directory")
	}
	// Joining nodes do not count: until a joiner completes its handshake (it
	// may never), the task needs a member that can receive work. A change that
	// does not reduce an already empty placement set (cancelling a join) stays
	// possible.
	if len(after.PlacementNodes()) == 0 && len(p.PlacementNodes()) > 0 {
		return rejectMembership("LAST_NODE", "At least one execution node must remain")
	}
	if len(after.Spec.Nodes) > 1024 {
		return errors.New("select between 1 and 1024 Agents")
	}
	consumers := len(after.ActiveNodes()) + len(after.Membership.Joining)
	if p.Spec.BandwidthMbps > 0 && math.Floor(p.Spec.BandwidthMbps*125000) < float64(consumers) {
		return rejectMembership("BANDWIDTH", "The task bandwidth limit is too small for the selected Agents")
	}
	if limits := p.Spec.RequestLimits; limits != nil && limits.Scope != "PER_NODE" && limits.BandwidthBitsPerSecond > 0 && limits.BandwidthBitsPerSecond/8/float64(consumers) < 1 {
		return rejectMembership("BANDWIDTH", "The task bandwidth limit is too small for the selected Agents")
	}
	return nil
}

func (p Parent) pendingRejection() error {
	var names []string
	for _, node := range p.Spec.Nodes {
		if p.NodeJoining(node.ID) {
			names = append(names, node.Address+" (joining)")
		} else if _, draining := p.NodeDraining(node.ID); draining {
			names = append(names, node.Address+" (leaving)")
		}
	}
	return rejectMembership("CHANGE_PENDING", "A previous node change is still in progress: %s. Wait for it to finish, cancel a join, re-add a leaving node, remove a draining node immediately, or remove a disconnected node", strings.Join(names, ", "))
}

// applyNodeChange returns a copy of p with the request applied. It does not
// validate; callers run CheckNodeChange first.
func (p Parent) applyNodeChange(change NodeChange, now time.Time) Parent {
	m := MembershipState{Joining: map[string]time.Time{}, Draining: map[string]NodeDrain{}, Removed: map[string]time.Time{}}
	if p.Membership != nil {
		m.Revision = p.Membership.Revision
		for k, v := range p.Membership.Joining {
			m.Joining[k] = v
		}
		for k, v := range p.Membership.Draining {
			m.Draining[k] = v
		}
		for k, v := range p.Membership.Removed {
			m.Removed[k] = v
		}
	}
	p.Membership = &m
	p.Spec.Nodes = append([]Node(nil), p.Spec.Nodes...)
	if p.Failover != nil {
		failover := *p.Failover
		failover.Rejoining = map[string]uint64{}
		for k, v := range p.Failover.Rejoining {
			failover.Rejoining[k] = v
		}
		p.Failover = &failover
	}
	retired := func(id string) bool {
		if p.Failover == nil {
			return false
		}
		_, ok := p.Failover.Retired[id]
		return ok
	}
	for _, node := range change.Add {
		index := -1
		for i, member := range p.Spec.Nodes {
			if member.ID == node.ID {
				index = i
			}
		}
		if _, draining := m.Draining[node.ID]; draining && index >= 0 {
			delete(m.Draining, node.ID)
			continue
		}
		if index >= 0 {
			// A removed node is fenced and all of its unfinished work has moved.
			// Re-adding it rejoins under a newer ownership epoch.
			p.Spec.Nodes[index] = node
			delete(m.Removed, node.ID)
		} else {
			p.Spec.Nodes = append(p.Spec.Nodes, node)
		}
		m.Joining[node.ID] = now
		// An added node must be able to receive work, otherwise it would only
		// dilute the working node's bandwidth/QPS share: affinity placement
		// switches to balanced placement when a node joins.
		p.Spec.DistributionMode = "BALANCED"
		if p.Spec.Direction == "LOCAL_TO_OSS" {
			p.Spec.SharedLocalSource = true
		}
	}
	for _, id := range change.Remove {
		switch _, draining := m.Draining[id]; {
		case p.NodeJoining(id):
			delete(m.Joining, id)
			if p.Failover != nil {
				delete(p.Failover.Rejoining, id)
			}
			if retired(id) || p.NodeEpoch(id) > 0 {
				m.Removed[id] = now
				continue
			}
			// The node never received authority or work: forget it entirely.
			nodes := p.Spec.Nodes[:0]
			for _, node := range p.Spec.Nodes {
				if node.ID != id {
					nodes = append(nodes, node)
				}
			}
			p.Spec.Nodes = nodes
		case draining:
			m.Draining[id] = NodeDrain{Mode: change.Mode, RequestedAt: m.Draining[id].RequestedAt}
		case retired(id):
			// Already fenced and taken over by failover; prevent its rejoin.
			m.Removed[id] = now
		default:
			m.Draining[id] = NodeDrain{Mode: change.Mode, RequestedAt: now}
		}
	}
	m.Revision++
	return p
}

// ChangeNodes durably accepts a membership change. A repeated request key with
// the same fingerprint returns the current parent without another change.
func (r *Repository) ChangeNodes(id int64, change NodeChange) (p Parent, replayed bool, err error) {
	if change.RequestKey == "" || len(change.RequestKey) > 256 {
		return p, false, errors.New("a request ID is required")
	}
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		var previous nodeRequestRecord
		found, err := tx.Get(nodeRequestBucket, nodeRequestKey(id, change.RequestKey), &previous)
		if err != nil {
			return err
		}
		if found {
			if previous.Fingerprint != change.Fingerprint {
				return ErrConflict
			}
			replayed = true
			return errUnchanged
		}
		if err := p.CheckNodeChange(change); err != nil {
			return err
		}
		if err := state.RequireSchema(tx, "master", state.SchemaTaskMembership); err != nil {
			return err
		}
		p = p.applyNodeChange(change, r.now().UTC())
		if err := validate(p.Spec); err != nil {
			return err
		}
		if err := r.save(tx, p); err != nil {
			return err
		}
		return tx.Put(nodeRequestBucket, nodeRequestKey(id, change.RequestKey), nodeRequestRecord{Fingerprint: change.Fingerprint, Revision: p.Membership.Revision})
	}))
	return
}

// NodeRequestSeen reports whether a request key was already accepted. A key
// reused for a different change is ErrConflict.
func (r *Repository) NodeRequestSeen(id int64, requestKey, fingerprint string) (seen bool, err error) {
	err = r.store.View(func(tx *state.Tx) error {
		var previous nodeRequestRecord
		found, err := tx.Get(nodeRequestBucket, nodeRequestKey(id, requestKey), &previous)
		if err != nil || !found {
			return err
		}
		if previous.Fingerprint != fingerprint {
			return ErrConflict
		}
		seen = true
		return nil
	})
	return
}

// DrainReady reports whether a draining node has no unfinished assignment and
// no queued handoff in flight, so that it can be fenced and leave.
func (r *Repository) DrainReady(id int64, nodeID string) (ready bool, err error) {
	err = r.store.View(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if _, draining := p.NodeDraining(nodeID); !draining {
			return nil
		}
		if p.Failover != nil && p.Failover.Rebalance != nil && p.Failover.Rebalance.Pending != nil && p.Failover.Rebalance.Pending.Source.ID == nodeID {
			return nil
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		ready = p.Scheduling.Nodes[nodeID].Children == 0
		return nil
	})
	return
}

// PendingChildren returns the derived unfinished-child count of every node.
func (p Parent) PendingChildren() map[string]int64 {
	if p.Scheduling == nil {
		return nil
	}
	counts := make(map[string]int64, len(p.Scheduling.Nodes))
	for id, load := range p.Scheduling.Nodes {
		counts[id] = load.Children
	}
	return counts
}

// EnsureDrainRebalance arms a queue-handoff plan while a draining node still
// owns unfinished work. Only children that never started can move.
func (r *Repository) EnsureDrainRebalance(id int64, generation uint64) (p Parent, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err = failoverParent(tx, id, generation)
		if err != nil {
			return err
		}
		if p.Failover.Rebalance != nil || p.Membership == nil || len(p.Membership.Draining) == 0 || p.Status != "RUNNING" {
			return errUnchanged
		}
		rebuilt := p.Scheduling == nil
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		ids := make([]string, 0, len(p.Membership.Draining))
		for nodeID := range p.Membership.Draining {
			ids = append(ids, nodeID)
		}
		sort.Strings(ids)
		owns := false
		for _, nodeID := range ids {
			owns = owns || p.Scheduling.Nodes[nodeID].Children > 0
		}
		if !owns {
			if rebuilt {
				return r.save(tx, p)
			}
			return errUnchanged
		}
		p.Failover.Rebalance = &RebalanceState{Targets: map[string]bool{}}
		return r.save(tx, p)
	}))
	return
}

// settleMembership ends pending changes of a completed task: no work remains,
// so draining nodes have left and joins are no longer needed.
func (p *Parent) settleMembership(now time.Time) {
	if !p.MembershipPending() {
		return
	}
	var remove []string
	for id := range p.Membership.Joining {
		remove = append(remove, id)
	}
	for id := range p.Membership.Draining {
		remove = append(remove, id)
	}
	sort.Strings(remove)
	revision := p.Membership.Revision
	for _, id := range remove {
		if _, draining := p.Membership.Draining[id]; draining {
			delete(p.Membership.Draining, id)
			if p.Membership.Removed == nil {
				p.Membership.Removed = map[string]time.Time{}
			}
			p.Membership.Removed[id] = now
		}
	}
	if len(p.Membership.Joining) > 0 {
		ids := make([]string, 0, len(p.Membership.Joining))
		for id := range p.Membership.Joining {
			ids = append(ids, id)
		}
		*p = p.applyNodeChange(NodeChange{Remove: ids, Mode: DrainModeDrain}, now)
	}
	p.Membership.Revision = revision + 1
}
