package master

import "github.com/MoonWX/oss-transfer/internal/task"

// Serialize the final retirement decision with installer maintenance admission.
// The earlier check avoids probing peers during planned downtime; this second
// check prevents a completed preflight from racing a newly admitted upgrade.
func (t *taskRuntime) retireUnavailableNode(p task.Parent, node task.Node) (task.Parent, error) {
	t.server.maintenanceMu.Lock()
	defer t.server.maintenanceMu.Unlock()
	protected, err := t.server.agentUnderMaintenance(node.ID)
	if err != nil || protected {
		return p, err
	}
	if err := t.leases.retire(p, node); err != nil {
		return p, err
	}
	return t.repo.BeginFailover(p.ID, p.Generation, node.ID)
}
