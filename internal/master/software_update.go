package master

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

type softwareUpdateState struct {
	mu        sync.Mutex
	latest    *softwareupdate.Release
	checkedAt time.Time
	checking  bool
	error     string
}

func installSoftwareUpdateRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/software-updates", s.adminOnly(s.softwareUpdatesHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/check", s.adminOnly(s.checkSoftwareUpdatesHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/master", s.adminOnly(s.updateMasterSoftwareHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/agents/{address}", s.adminOnly(s.updateAgentSoftwareHTTP))
}

func (s *Server) localSoftware() (softwareupdate.Info, softwareupdate.Status) {
	if s.options.Updater == nil {
		return softwareupdate.Info{Role: "master", Reason: "UNMANAGED"}, softwareupdate.Status{}
	}
	return s.options.Updater.Info(), s.options.Updater.Status()
}

func (s *Server) softwareUpdatesHTTP(w http.ResponseWriter, r *http.Request) {
	info, status := s.localSoftware()
	replies, err := s.nodeReplies(r.Context(), "software_info")
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	displays := s.nodeDisplaySnapshot()
	nodes := make([]map[string]any, 0, len(replies))
	for _, reply := range replies {
		row := map[string]any{"address": reply.Member.Address, "info": nil, "update": nil}
		addNodeDisplay(row, displays.lookup(reply.Member.ID, reply.Member.Address))
		if reply.Err != nil {
			row["unavailable"] = true
		} else {
			row["info"], row["update"] = reply.Value["info"], reply.Value["update"]
		}
		nodes = append(nodes, row)
	}
	s.updates.mu.Lock()
	latest, checked, checking, checkError := s.updates.latest, s.updates.checkedAt, s.updates.checking, s.updates.error
	s.updates.mu.Unlock()
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{
		"master": map[string]any{"info": info, "update": status}, "agents": nodes,
		"latest": latest, "checkedAt": checked, "checking": checking, "checkError": checkError,
		"available": latest != nil && newerSoftware(latest.Version, info.Version),
	}})
}

func (s *Server) checkSoftwareUpdatesHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !readJSON(w, r, &body) {
		return
	}
	if s.options.Updater == nil {
		fail(w, 409, "Version checking is unavailable")
		return
	}
	s.updates.mu.Lock()
	if s.updates.checking {
		s.updates.mu.Unlock()
		fail(w, 409, "A version check is already running")
		return
	}
	s.updates.checking = true
	s.updates.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	release, err := s.options.Updater.Check(ctx)
	s.updates.mu.Lock()
	s.updates.checking, s.updates.error = false, ""
	if err == nil {
		s.updates.latest, s.updates.checkedAt = &release, time.Now().UTC()
	} else {
		s.updates.error = "Version check failed; try again"
	}
	s.updates.mu.Unlock()
	if err != nil {
		fail(w, 502, "Version check failed; try again")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": release})
}

func (s *Server) updateMasterSoftwareHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	s.updates.mu.Lock()
	checked := s.updates.latest != nil && s.updates.latest.Version == request.Version && time.Since(s.updates.checkedAt) < time.Hour
	s.updates.mu.Unlock()
	if !checked || s.options.Updater == nil {
		fail(w, 409, "Check the latest version before updating")
		return
	}
	result, err := s.options.Updater.Start(r.Context(), request.Version)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "data": result})
}

func (s *Server) updateAgentSoftwareHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	master, masterUpdate := s.localSoftware()
	if request.Version == "" || request.Version != master.Version || updateInProgress(masterUpdate.Phase) {
		fail(w, 409, "Update Agents to the current Master version after the Master update finishes")
		return
	}
	member, err := s.nodeByAddress(r.PathValue("address"))
	if err != nil {
		nodeHTTPError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var result softwareupdate.Status
	err = s.agentCall(ctx, member.Session(), "software_update", true, request, &result)
	if err != nil {
		fail(w, 409, "Cannot start the Agent update; check its connection and update support")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "data": result})
}

func updateInProgress(phase string) bool {
	return phase == "QUEUED" || phase == "DOWNLOADING" || phase == "APPLYING"
}

// Stable versions compare numerically; a development build is never silently downgraded.
func newerSoftware(target, current string) bool {
	parse := func(v string) ([3]int, bool) {
		var out [3]int
		parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	a, ok := parse(target)
	b, oldOK := parse(current)
	if !ok || !oldOK {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
