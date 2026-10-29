package master

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/MoonWX/oss-transfer/internal/backupretention"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

func backupStats(ctx context.Context, updater softwareupdate.Controller) softwareupdate.BackupSummary {
	controller, ok := updater.(softwareupdate.BackupController)
	if !ok {
		return softwareupdate.BackupSummary{Reason: "UNMANAGED"}
	}
	value, err := controller.BackupStats(ctx)
	if err != nil {
		value = softwareupdate.BackupSummary{Reason: "ERROR"}
		if errors.Is(err, backupretention.ErrBusy) {
			value.Reason = "BUSY"
		}
	}
	return value
}

func (s *Server) backupsHTTP(w http.ResponseWriter, r *http.Request) {
	local := backupStats(r.Context(), s.options.Updater)
	replies, err := s.nodeReplies(r.Context(), "backup_list")
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	displays := s.nodeDisplaySnapshot()
	nodes := make([]map[string]any, 0, len(replies))
	for _, reply := range replies {
		row := reply.Value
		if reply.Err != nil || row == nil {
			reason := "OFFLINE"
			var remote *AgentError
			if errors.As(reply.Err, &remote) {
				reason = "LEGACY"
			}
			row = map[string]any{"supported": false, "reason": reason, "count": 0, "bytes": 0, "reclaimableCount": 0, "reclaimableBytes": 0}
		}
		row["address"] = reply.Member.Address
		addNodeDisplay(row, displays.lookup(reply.Member.ID, reply.Member.Address))
		nodes = append(nodes, row)
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{
		"policy": map[string]int{"keep": 2, "maxAgeDays": 30}, "master": local, "agents": nodes,
	}})
}

func (s *Server) pruneMasterBackupsHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !readJSON(w, r, &body) {
		return
	}
	controller, ok := s.options.Updater.(softwareupdate.BackupController)
	if !ok {
		fail(w, 409, "Backup maintenance is unavailable")
		return
	}
	result, err := controller.PruneBackups(r.Context())
	if err != nil {
		fail(w, 409, "Backup cleanup could not finish; refresh and try again")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": result})
}

func (s *Server) pruneAgentBackupsHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !readJSON(w, r, &body) {
		return
	}
	member, err := s.nodeByAddress(r.PathValue("address"))
	if err != nil {
		nodeHTTPError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var result softwareupdate.BackupCleanup
	if err = s.agentCall(ctx, member.Session(), "backup_prune", true, struct{}{}, &result); err != nil {
		fail(w, 409, "Backup cleanup could not finish; refresh and try again")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": result})
}
