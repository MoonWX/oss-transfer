package master

import (
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type taskRequestLimitView struct {
	Scope string  `json:"scope,omitempty"`
	List  float64 `json:"list"`
	Head  float64 `json:"head"`
	Put   float64 `json:"put"`
}

// addRequestRates reads only the cached metrics sample. All limits use the
// task-limit convention that zero means unlimited; a nil part is unknown.
func (s *Server) addRequestRates(row map[string]any, p task.Parent) {
	view := map[string]any{"observed": nil, "configured": nil, "effective": nil}
	row["requestRates"] = view
	nodes := p.ActiveNodes()
	if limits := p.Spec.RequestLimits; limits != nil && len(nodes) > 0 {
		// Report task totals whatever the stored scope, so PER_NODE limits on
		// three Agents read as three times the per-node value.
		members := 1.0
		if limits.Scope == "PER_NODE" {
			members = float64(len(nodes))
		}
		view["configured"] = taskRequestLimitView{Scope: limits.Scope, List: limits.ListQPS * members, Head: limits.HeadQPS * members, Put: limits.PutQPS * members}
	}
	if task.Ended(p.Status) {
		view["observed"] = nodecontrol.TaskRequestRates{}
		return
	}
	entry, fresh := s.tasks.rateSnapshot(p)
	if !fresh || entry.requests == nil {
		return
	}
	view["observed"] = *entry.requests
	if effective, ok := effectiveRequestLimits(p, entry.nodeLimits); ok {
		view["effective"] = effective
	}
}

// effectiveRequestLimits is an upper bound: node limits are shared with other
// tasks on the same Agent. Each node admits at most min(task share, node
// limit), where either side may be unlimited.
func effectiveRequestLimits(p task.Parent, nodeLimits map[string]nodecontrol.NodeRequestLimits) (taskRequestLimitView, bool) {
	nodes := p.ActiveNodes()
	if len(nodes) == 0 {
		return taskRequestLimitView{}, false
	}
	bound := func(share, node float64) float64 {
		switch {
		case share == 0:
			return node
		case node == 0:
			return share
		}
		return min(share, node)
	}
	var sums [3]float64
	var unlimited [3]bool
	for index, node := range nodes {
		limits, ok := nodeLimits[node.ID]
		if !ok {
			// Membership changed since the sample; the bound is unknown.
			return taskRequestLimitView{}, false
		}
		policy := parentPolicy(p, index)
		// The node LIST budget never admits GetObject, so only the task share
		// bounds the shared LIST / GET bucket.
		for kind, value := range [3]float64{policy.ListQPS, bound(policy.HeadQPS, limits.Head), bound(policy.PutQPS, limits.Put)} {
			sums[kind] += value
			unlimited[kind] = unlimited[kind] || value == 0
		}
	}
	for kind := range sums {
		if unlimited[kind] {
			sums[kind] = 0
		}
	}
	return taskRequestLimitView{List: sums[0], Head: sums[1], Put: sums[2]}, true
}
