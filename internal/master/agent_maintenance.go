package master

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const agentMaintenanceTTL = 30 * time.Minute

type agentMaintenance struct {
	ID           string    `json:"id"`
	EnrollmentID string    `json:"enrollmentId"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Ended        bool      `json:"ended,omitempty"`
}

// Only the installed Agent's private local grant can open its maintenance
// window. A caller cannot supply another node ID or prolong an existing window.
func (s *Server) agentMaintenanceHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Action string `json:"action"`
		ID     string `json:"id"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	id, err := hex.DecodeString(request.ID)
	if err != nil || len(id) != 32 || (request.Action != "begin" && request.Action != "end") {
		fail(w, 400, "Invalid Agent maintenance operation")
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	user, valid, err := s.authenticated(r)
	if err != nil {
		fail(w, 500, "Cannot authenticate Agent maintenance")
		return
	}
	if !valid || !user.LocalCLI || !strings.HasPrefix(user.Username, "local:agent:") {
		fail(w, 403, "Agent local access is required for upgrade maintenance")
		return
	}
	nodeID := strings.TrimPrefix(user.Username, "local:agent:")
	conflict := errors.New("maintenance conflict")
	var result agentMaintenance
	err = s.store.Update(func(tx *state.Tx) error {
		var member cluster.Member
		found, err := tx.Get("agent-members", nodeID, &member)
		if err != nil {
			return err
		}
		if !found || member.Revoked {
			return conflict
		}
		found, err = tx.Get("agent-maintenance", nodeID, &result)
		if err != nil {
			return err
		}
		now := s.options.Now().UTC()
		if request.Action == "end" {
			if !found || result.ID != request.ID || result.EnrollmentID != member.EnrollmentID {
				return conflict
			}
			result.Ended = true
		} else if found && result.ID == request.ID {
			// Replays acknowledge the original deadline, never renew it.
			if result.Ended || result.EnrollmentID != member.EnrollmentID || !now.Before(result.ExpiresAt) {
				return conflict
			}
		} else {
			if found && !result.Ended && result.EnrollmentID == member.EnrollmentID && now.Before(result.ExpiresAt) {
				return conflict
			}
			result = agentMaintenance{ID: request.ID, EnrollmentID: member.EnrollmentID, ExpiresAt: now.Add(agentMaintenanceTTL)}
		}
		return tx.Put("agent-maintenance", nodeID, result)
	})
	if errors.Is(err, conflict) {
		fail(w, 409, "Another Agent maintenance operation is active or this operation has expired")
		return
	}
	if err != nil {
		fail(w, 500, "Cannot save Agent maintenance")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "id": result.ID, "expiresAt": result.ExpiresAt})
}

// agentUnderMaintenance suppresses failure takeover, not execution fencing.
// Leases still expire while the Agent is stopped. The bounded durable window
// survives a Master restart without leaving a failed installer protected forever.
func (s *Server) agentUnderMaintenance(nodeID string) (bool, error) {
	active := false
	err := s.store.View(func(tx *state.Tx) error {
		var maintenance agentMaintenance
		found, err := tx.Get("agent-maintenance", nodeID, &maintenance)
		if err != nil || !found || maintenance.Ended || !s.options.Now().Before(maintenance.ExpiresAt) {
			return err
		}
		var member cluster.Member
		found, err = tx.Get("agent-members", nodeID, &member)
		active = found && !member.Revoked && member.EnrollmentID == maintenance.EnrollmentID
		return err
	})
	if err != nil || active {
		return active, err
	}
	queue, err := s.softwareQueue()
	if err != nil {
		return false, err
	}
	for _, item := range queue.Items {
		if item.NodeID == nodeID && item.Phase == "UPDATING" && !item.StartedAt.IsZero() && s.options.Now().Before(item.StartedAt.Add(agentMaintenanceTTL)) {
			return true, nil
		}
	}
	return false, nil
}
