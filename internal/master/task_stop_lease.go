package master

import "github.com/MoonWX/oss-transfer/internal/task"

// A lease expiry fences both discovery and transfer I/O. During a user stop,
// it can replace an unreachable owner's stop acknowledgement; it cannot prove
// any additional object completed. Keep the last observed object ledger.
// renew rechecks the durable parent while holding the same entry lock used by
// expired, so a stale RUNNING snapshot cannot grant new authority afterward.
func (t *taskRuntime) nodeLeaseStopped(p task.Parent, node task.Node) (bool, error) {
	if (p.Status != "STOPPING" && p.Status != "STOPPED") || p.Failover == nil || !p.Failover.Enabled {
		return false, nil
	}
	if p.NodeRetired(node.ID) {
		return true, nil
	}
	return t.leases.expired(p, node)
}
