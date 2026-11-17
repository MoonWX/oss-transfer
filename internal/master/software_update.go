package master

import (
	"context"
	"errors"
	"log"
	"net/http"
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

	// errorDetail is the reason each release source gave for the last
	// failed check, and failedAt when it failed. A successful check clears
	// both; checkedAt records only successful checks.
	errorDetail string
	failedAt    time.Time
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

// reachedTarget reports whether a node running version has reached target,
// the version an update attempt was meant to install. Unparseable versions,
// such as development builds, never have.
func reachedTarget(version, target string) bool {
	result, ok := softwareupdate.CompareVersions(strings.TrimPrefix(version, "v"), strings.TrimPrefix(target, "v"))
	return ok && result >= 0
}

// currentUpdate hides a failed attempt whose target the node already runs, or
// has passed, for example after an operator upgraded it with the installer as
// the failure advised. The status file keeps the old failure until the next
// online update, and nothing in the console could clear it. The persisted
// status, which the update queue reconciles against, is unchanged.
func currentUpdate(status softwareupdate.Status, version string) softwareupdate.Status {
	if status.Phase == "FAILED" && reachedTarget(version, status.Version) {
		return softwareupdate.Status{Phase: "IDLE"}
	}
	return status
}

func (s *Server) softwareUpdatesHTTP(w http.ResponseWriter, r *http.Request) {
	info, status := s.localSoftware()
	status = currentUpdate(status, info.Version)
	// Capability details explain why online updates are unavailable and what
	// to do instead; show them in the requested language.
	info.Detail = localize(w, info.Detail)
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
		if hostname := s.nodeHostname(reply.Member.ID); hostname != "" {
			// A container Agent added with the Docker command is named after
			// its hostname; the console uses it in the update commands.
			row["hostname"] = hostname
		}
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
				if reason, _ := agent["reason"].(string); reason == "CONTAINER" {
					// Older Agents word this differently ("Container
					// installations must be updated with Docker Compose");
					// every container is updated the same way.
					agent["detail"] = localize(w, softwareupdate.ContainerUpdateMessage)
				} else if detail, ok := agent["detail"].(string); ok {
					agent["detail"] = localize(w, detail)
				}
				version, _ := agent["version"].(string)
				if update, ok := reply.Value["update"].(map[string]any); ok {
					// The Agent's own status file follows the same rule.
					phase, _ := update["phase"].(string)
					target, _ := update["version"].(string)
					if phase == "FAILED" && reachedTarget(version, target) {
						row["update"] = softwareupdate.Status{Phase: "IDLE"}
					}
				}
				row["updateAvailable"] = newerSoftware(info.Version, version)
				row["upgradeReason"] = agent["reason"]
			}
		}
		nodes = append(nodes, row)
	}
	s.updates.mu.Lock()
	latest, checked, checking, checkError := s.updates.latest, s.updates.checkedAt, s.updates.checking, s.updates.error
	checkErrorDetail, checkFailed := s.updates.errorDetail, s.updates.failedAt
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
	data := map[string]any{
		"master": map[string]any{"info": info, "update": status}, "agents": nodes,
		"latest": latest, "checkedAt": checked, "checking": checking, "checkError": localize(w, checkError),
		"available": latest != nil && newerSoftware(latest.Version, info.Version),
		"queue":     queue,
		// Container Agents are updated by recreating them from this image.
		"agentImage": s.options.AgentDockerImage,
	}
	if checkError != "" {
		// The console does not check again on its own soon after a failure.
		data["checkErrorDetail"], data["checkFailedAt"] = checkErrorDetail, checkFailed
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}

// Why a version check failed: no release source answered, so checking again
// cannot help until the Master can reach one, or a source answered with an
// error.
const (
	versionSourcesUnreachable = `The Master cannot reach GitHub or JD Cloud OSS to check for new versions. Checking again will not succeed until it can reach one of them; to upgrade without them, use an offline package (see "Offline installation" in the documentation)`
	versionCheckFailed        = "Version check failed: the release sources did not return a valid release"
)

// versionCheckFailure explains a failed version check and gives the reason
// each release source gave, one source per line.
func versionCheckFailure(err error) (message, detail string) {
	detail = strings.TrimSpace(err.Error())
	if softwareupdate.SourcesUnreachable(err) {
		return versionSourcesUnreachable, detail
	}
	return versionCheckFailed, detail
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
	message, detail := "", ""
	if err != nil {
		message, detail = versionCheckFailure(err)
		log.Printf("version check failed: %s", strings.ReplaceAll(detail, "\n", "; "))
	}
	s.updates.mu.Lock()
	s.updates.checking, s.updates.error, s.updates.errorDetail = false, message, detail
	if err == nil {
		s.updates.latest, s.updates.checkedAt, s.updates.failedAt = &release, time.Now().UTC(), time.Time{}
	} else {
		s.updates.failedAt = time.Now().UTC()
	}
	s.updates.mu.Unlock()
	if err != nil {
		writeJSON(w, 502, map[string]any{"success": false, "message": localize(w, message), "detail": detail})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": release})
}

func (s *Server) updateMasterSoftwareHTTP(w http.ResponseWriter, r *http.Request) {
	// Normal updates install the checked latest stable release. An explicit
	// version (the console's hidden prerelease entry or the CLI) requires
	// allowPrerelease and is verified against published metadata instead.
	var request struct {
		Version         string `json:"version"`
		AllowPrerelease bool   `json:"allowPrerelease"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if softwareupdate.IsPrerelease(request.Version) && !request.AllowPrerelease {
		fail(w, 400, softwareupdate.ErrPrereleaseNotAllowed.Error())
		return
	}
	// The opt-in channel installs prereleases only. A stable version always
	// takes the normal path, which requires a recent check of the latest
	// release, even if the client also sent the prerelease flag.
	if request.AllowPrerelease && softwareupdate.ValidVersion(request.Version) && !softwareupdate.IsPrerelease(request.Version) {
		request.AllowPrerelease = false
	}
	if request.AllowPrerelease {
		resolver, ok := s.options.Updater.(softwareupdate.VersionResolver)
		if !ok || !s.options.Updater.Info().Supported {
			fail(w, 409, "Online updates are unavailable for this installation")
			return
		}
		if !softwareupdate.ValidVersion(request.Version) {
			fail(w, 400, softwareupdate.ErrInvalidVersion.Error())
			return
		}
		if !softwareupdate.Newer(request.Version, s.options.Updater.Info().Version) {
			fail(w, 409, softwareupdate.ErrNotNewer.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		_, err := resolver.Resolve(ctx, request.Version, true)
		cancel()
		if err != nil {
			fail(w, 502, "The requested release could not be verified; check the version and try again")
			return
		}
	} else {
		s.updates.mu.Lock()
		checked := s.updates.latest != nil && s.updates.latest.Version == request.Version && time.Since(s.updates.checkedAt) < time.Hour
		s.updates.mu.Unlock()
		if !checked || s.options.Updater == nil {
			fail(w, 409, "Check the latest version before updating")
			return
		}
	}
	s.updates.queueMu.Lock()
	defer s.updates.queueMu.Unlock()
	if queue, err := s.softwareQueue(); err != nil || queue.Phase == "RUNNING" {
		fail(w, 409, "Wait for Agent updates to finish before updating the Master")
		return
	}
	result, err := s.options.Updater.Start(r.Context(), request.Version, request.AllowPrerelease)
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

// Versions compare by SemVer precedence (4.1.29-beta.1 < 4.1.29); a
// development build is never silently replaced or downgraded.
func newerSoftware(target, current string) bool {
	return softwareupdate.Newer(strings.TrimPrefix(target, "v"), strings.TrimPrefix(current, "v"))
}
