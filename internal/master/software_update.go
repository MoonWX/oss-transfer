package master

import (
	"context"
	"errors"
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
	queueMu   sync.Mutex
	lifecycle sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	closed    bool
}

func installSoftwareUpdateRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/software-updates/backups", s.adminOnly(s.backupsHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/backups/master/prune", s.adminOnly(s.pruneMasterBackupsHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/backups/agents/{address}/prune", s.adminOnly(s.pruneAgentBackupsHTTP))
	s.mux.HandleFunc("GET /api/master/software-updates", s.adminOnly(s.softwareUpdatesHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/check", s.adminOnly(s.checkSoftwareUpdatesHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/master", s.adminOnly(s.updateMasterSoftwareHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/agents/{address}", s.adminOnly(s.updateAgentSoftwareHTTP))
	s.mux.HandleFunc("POST /api/master/software-updates/agents", s.adminOnly(s.updateAgentSoftwareHTTP))
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
			// Older Agents do not implement software_info. Establish reachability
			// independently so an unsupported updater is not labelled offline.
			var remoteError *AgentError
			if !errors.As(reply.Err, &remoteError) {
				row["unavailable"], row["upgradeReason"] = true, "OFFLINE"
				nodes = append(nodes, row)
				continue
			}
			var legacy map[string]any
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			err := s.agentCall(ctx, reply.Member.Session(), "node_status", false, struct{}{}, &legacy)
			cancel()
			if err == nil {
				row["info"] = softwareupdate.Info{Role: "agent", Reason: "LEGACY"}
				if info, ok := legacy["software"].(map[string]any); ok {
					row["info"] = info
				}
				row["upgradeReason"] = "LEGACY"
			} else {
				row["unavailable"], row["upgradeReason"] = true, "OFFLINE"
			}
		} else {
			row["info"], row["update"] = reply.Value["info"], reply.Value["update"]
			if agent, ok := reply.Value["info"].(map[string]any); ok {
				version, _ := agent["version"].(string)
				row["updateAvailable"] = newerSoftware(info.Version, version)
				row["upgradeReason"] = agent["reason"]
			}
		}
		nodes = append(nodes, row)
	}
	s.updates.mu.Lock()
	latest, checked, checking, checkError := s.updates.latest, s.updates.checkedAt, s.updates.checking, s.updates.error
	s.updates.mu.Unlock()
	queue, _ := s.softwareQueue()
	for i := range queue.Items {
		queue.Items[i].Error = localize(w, queue.Items[i].Error)
	}
	for _, row := range nodes {
		for _, item := range queue.Items {
			if row["address"] == item.Address {
				row["queuePhase"] = item.Phase
			}
		}
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{
		"master": map[string]any{"info": info, "update": status}, "agents": nodes,
		"latest": latest, "checkedAt": checked, "checking": checking, "checkError": localize(w, checkError),
		"available": latest != nil && newerSoftware(latest.Version, info.Version),
		"queue":     queue,
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
	s.updates.queueMu.Lock()
	defer s.updates.queueMu.Unlock()
	if queue, err := s.softwareQueue(); err != nil || queue.Phase == "RUNNING" {
		fail(w, 409, "Wait for Agent updates to finish before updating the Master")
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
	result, err := s.enqueueAgentUpdates(r.Context(), request.Version, r.PathValue("address"))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	for i := range result.Items {
		result.Items[i].Error = localize(w, result.Items[i].Error)
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
