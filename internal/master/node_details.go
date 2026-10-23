package master

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) agentNodeDetails(w http.ResponseWriter, r *http.Request) {
	member, err := s.nodeByAddress(r.PathValue("address"))
	if err != nil {
		nodeHTTPError(w, err)
		return
	}
	s.agents.mu.Lock()
	seen := s.agents.lastSeen[member.ID]
	remote := s.agents.remoteAddress[member.ID]
	s.agents.mu.Unlock()
	data := map[string]any{
		"id": member.ID, "address": member.Address, "label": member.Label,
		"online":            !seen.IsZero() && s.options.Now().Sub(seen) < 75*time.Second,
		"connectionAddress": remote, "complete": false,
	}
	if !seen.IsZero() {
		data["lastSeen"] = seen
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var details map[string]any
	err = s.agentCall(ctx, member.Session(), "node_details", false, struct{}{}, &details)
	if err != nil {
		// Retain identity and the last observed peer address when disconnected.
		// An older Agent may not implement details yet.
		data["message"] = localize(w, "Node details are unavailable")
	} else {
		for _, key := range []string{"hostname", "os", "arch", "localIps", "advertisedLocalRoots", "mounts", "software"} {
			data[key] = details[key]
		}
		data["complete"] = true
		data["online"] = true
	}
	s.rememberNodeInfo(member.ID, data)
	addNodeDisplay(data, s.nodeDisplaySnapshot().lookup(member.ID, member.Address))
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}
