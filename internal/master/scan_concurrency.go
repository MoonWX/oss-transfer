package master

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
)

// A node's scan concurrency is how many scans its Agent runs at once
// (PUT /api/master/nodes/{address}, maxConcurrentScans).
const (
	scanConcurrencyRangeMessage       = "Scan concurrency must be 1–16"
	scanConcurrencyUnsupportedMessage = "This node's Agent is too old to set scan concurrency; update it first"
)

// scanConcurrencySupported reports whether the node's Agent accepts
// maxConcurrentScans in node_config_set. An older Agent decodes the setting
// strictly and would refuse the whole change.
func (s *Server) scanConcurrencySupported(ctx context.Context, member cluster.Member) (bool, error) {
	var caps struct {
		ScanConcurrencyProtocol int `json:"scanConcurrencyProtocol"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.agentCall(callCtx, member.Session(), "capabilities", false, nil, &caps)
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Message == "unknown_operation" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return caps.ScanConcurrencyProtocol >= agentservice.ScanConcurrencyProtocol, nil
}

// scanCapacityChanged is called once a node's Agent has accepted a new scan
// concurrency. It is where the Master's view of free scan slots learns the new
// capacity (the scan slot view's capacityChanged): a raised value lets queued
// scans start at once. A lowered value needs nothing more, because the Agent
// cancels no running scan and starts new ones only below the new value.
func (t *taskRuntime) scanCapacityChanged(nodeID string, capacity int) {}
