package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type taskNodesRequest struct {
	RequestID            string        `json:"requestId"`
	Add                  []string      `json:"add"`
	Remove               []string      `json:"remove"`
	Bindings             []taskBinding `json:"bindings"`
	Mode                 string        `json:"mode"`
	ExpectedGeneration   *uint64       `json:"expectedGeneration"`
	ExpectedNodeRevision *uint64       `json:"expectedNodeRevision"`
	DryRun               bool          `json:"dryRun"`
	// SkipSharedCheck skips proving that the added nodes' local paths see
	// the task's shared directory, as at creation: it is refused unless
	// SkipSharedCheckAcknowledged is also true, and ignored unless the change
	// adds nodes to a local transfer. Only the added nodes go unverified;
	// the runtime keeps verifying the other nodes. Without it the added
	// nodes are verified, even if the task was created with the check
	// skipped.
	SkipSharedCheck             bool `json:"skipSharedCheck,omitempty"`
	SkipSharedCheckAcknowledged bool `json:"skipSharedCheckAcknowledged,omitempty"`
}

func installTaskMembershipRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/tasks/{id}/nodes", s.adminOnly(s.taskNodesHTTP))
	s.mux.HandleFunc("POST /api/master/tasks/{id}/nodes", s.adminOnly(s.changeTaskNodesHTTP))
}

func (s *Server) taskNodesHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	p, err := s.tasks.repo.Get(id)
	if errors.Is(err, task.ErrNotFound) || err == nil && (p.Purged || p.Deleted) {
		fail(w, 404, "Task not found")
		return
	}
	if err != nil {
		fail(w, 500, "Cannot read task")
		return
	}
	view, err := s.taskNodesView(r.Context(), p)
	if err != nil {
		fail(w, 500, "Cannot read execution Agents")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": view})
}

type membershipFailure struct {
	status   int
	code     string
	message  string
	bindings []bindingCheck
}

func (f *membershipFailure) Error() string { return f.message }

func membershipReject(ctx context.Context, code, format string, args ...any) *membershipFailure {
	return &membershipFailure{status: 409, code: code, message: fmt.Sprintf(localizeContext(ctx, format), args...)}
}

func (s *Server) changeTaskNodesHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var request taskNodesRequest
	if !readJSON(w, r, &request) {
		return
	}
	result, failure := s.changeTaskNodes(r.Context(), id, request, r.Header.Get("Idempotency-Key"))
	if failure != nil {
		body := map[string]any{"success": false, "message": failure.message}
		if failure.code != "" {
			body["code"] = failure.code
		}
		if failure.bindings != nil {
			body["nodeBindings"] = failure.bindings
		}
		writeJSON(w, failure.status, body)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": result})
}

func (s *Server) changeTaskNodes(ctx context.Context, id int64, request taskNodesRequest, header string) (map[string]any, *membershipFailure) {
	bad := func(message string) *membershipFailure {
		return &membershipFailure{status: 400, message: localizeContext(ctx, message)}
	}
	requestKey := strings.TrimSpace(request.RequestID)
	if header != "" {
		if requestKey != "" && requestKey != header {
			return nil, bad("requestId conflicts with Idempotency-Key")
		}
		requestKey = header
	}
	if len(requestKey) > 220 {
		return nil, bad("Request ID is too long")
	}
	mode := task.DrainModeDrain
	switch strings.ToLower(request.Mode) {
	case "", "drain":
	case "now":
		mode = task.DrainModeNow
	default:
		return nil, bad("Removal mode must be drain or now")
	}
	add, err := trimmedList(request.Add)
	if err != nil {
		return nil, bad(err.Error())
	}
	remove, err := trimmedList(request.Remove)
	if err != nil {
		return nil, bad(err.Error())
	}
	if len(add)+len(remove) == 0 {
		return nil, bad("Select execution nodes to add or remove")
	}
	// As at creation, skipping the check needs the acknowledgement of its
	// consequences, which the refusal names.
	if request.SkipSharedCheck && !request.SkipSharedCheckAcknowledged {
		return nil, membershipReject(ctx, "SHARED_CHECK_SKIP_UNACKNOWLEDGED", skipSharedCheckUnacknowledged)
	}
	bindings := map[string]string{}
	bindingKeys := []string{}
	for _, binding := range request.Bindings {
		address := strings.TrimSpace(binding.ExecutionNodeIP)
		if address == "" || bindings[address] != "" {
			return nil, bad("Invalid or duplicate local node binding")
		}
		bindings[address] = binding.LocalPath
		bindingKeys = append(bindingKeys, address+"="+binding.LocalPath)
	}
	sort.Strings(bindingKeys)
	identity := map[string]any{"add": normalizedMembers(add), "remove": normalizedMembers(remove), "bindings": bindingKeys, "mode": mode, "generation": request.ExpectedGeneration, "revision": request.ExpectedNodeRevision}
	if request.SkipSharedCheck {
		// Only when set, so that other requests keep their fingerprint.
		identity["skipSharedCheck"] = true
	}
	fingerprint := digestValue(identity)
	p, err := s.tasks.repo.Get(id)
	if errors.Is(err, task.ErrNotFound) || err == nil && p.Purged {
		return nil, &membershipFailure{status: 404, message: localizeContext(ctx, "Task not found")}
	}
	if err != nil {
		return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read task")}
	}
	// A seen key replays even for a dry run, so a client retrying an
	// unknown outcome is never told that its accepted change failed preflight.
	if requestKey != "" {
		seen, err := s.tasks.repo.NodeRequestSeen(id, requestKey, fingerprint)
		if errors.Is(err, task.ErrConflict) {
			return nil, membershipReject(ctx, "CONFLICT", "This request ID was already used for a different node change")
		}
		if err != nil {
			return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read task")}
		}
		if seen {
			return s.membershipResult(ctx, p.ID, true, true, false, true, nil, nil)
		}
	}
	if blocked := p.MembershipBlocked(); blocked != nil {
		return nil, s.membershipError(ctx, blocked)
	}
	// Resolve identities. Removals name current members; additions name online
	// enrolled Agents (or a draining member whose drain is cancelled).
	byKey := map[string]task.Node{}
	for _, node := range p.Spec.Nodes {
		byKey[node.ID], byKey[node.Address] = node, node
	}
	change := task.NodeChange{Mode: mode, ExpectedGeneration: request.ExpectedGeneration, ExpectedRevision: request.ExpectedNodeRevision, Fingerprint: fingerprint}
	for _, value := range remove {
		node, ok := byKey[value]
		if !ok || p.NodeRemoved(node.ID) {
			return nil, membershipReject(ctx, "NOT_MEMBER", "Agent %s is not an execution node of this task", value)
		}
		change.Remove = append(change.Remove, node.ID)
	}
	members, err := s.onlineAgents()
	if err != nil {
		return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read execution Agents")}
	}
	online := map[string]cluster.Member{}
	for _, member := range members {
		online[member.ID], online[member.Address] = member, member
	}
	displays := s.nodeDisplaySnapshot()
	label := func(node task.Node) string { return displays.lookup(node.ID, node.Address).DisplayLabel }
	local := p.Spec.Source.Kind == "LOCAL" || p.Spec.Destination.Kind == "LOCAL"
	var joining []task.Node
	kept := 0
	for _, value := range add {
		if node, ok := byKey[value]; ok {
			if _, draining := p.NodeDraining(node.ID); draining {
				// Cancelling a removal keeps the node's binding. Clients may omit
				// it or repeat the existing path; a different path is rejected.
				change.Add = append(change.Add, node)
				path, bound := bindings[node.Address]
				if !bound {
					path, bound = bindings[node.ID]
				}
				if bound {
					if path != node.LocalRoot {
						return nil, bad("A draining node keeps its existing binding")
					}
					kept++
				}
				continue
			}
		}
		member, ok := online[value]
		if !ok {
			return nil, membershipReject(ctx, "AGENT_OFFLINE", "Agent %s is not online in this enrollment", displays.lookup(value, value).DisplayLabel)
		}
		node := task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address}
		if local {
			path, bound := bindings[member.Address]
			if !bound {
				path, bound = bindings[member.ID]
			}
			if !bound || path == "" {
				return nil, membershipReject(ctx, "BINDING_REQUIRED", "Agent %s needs a local path bound to the shared directory", label(node))
			}
			node.LocalRoot = path
		}
		change.Add = append(change.Add, node)
		joining = append(joining, node)
	}
	if !local && len(bindings) > 0 {
		return nil, bad("Only local transfers accept node bindings")
	}
	if local && len(bindings) != len(joining)+kept {
		return nil, bad("Provide exactly one local path for each added node")
	}
	// The operator's choice covers the local paths this change adds. A
	// change that adds none ignores it and records nothing, as creation
	// ignores it for a single node.
	skip := request.SkipSharedCheck && local && len(joining) > 0
	change.SkipSharedCheck = skip
	if err := p.CheckNodeChange(change); err != nil {
		return nil, s.membershipError(ctx, err)
	}
	// The runtime grants a proof without a check (shared_proof_skip) to the
	// nodes this change adds with the check skipped, and to every node of a
	// task created with the check skipped, so each of these must support it.
	// The current members keep their verification.
	joinSkipped := local && len(joining) > 0 && (skip || p.SharedCheckSkippedForEveryNode())
	// Mixed versions: every joining node and every reachable current member
	// must support fencing and queue handoff, as for automatic takeover.
	for _, node := range joining {
		if s.softwareUpdateBlocksNode(node.ID) {
			return nil, membershipReject(ctx, "AGENT_UPDATING", "Agent %s is updating; try again after the update", label(node))
		}
		caps, err := s.tasks.capabilities(ctx, node)
		if err != nil {
			return nil, membershipReject(ctx, "AGENT_VERSION_UNAVAILABLE", capabilitiesUnreadableMessage, label(node), err.Error())
		}
		if !joinCapable(caps) || !conflictPolicyCapable(p.Spec.ConflictPolicy, caps) {
			return nil, membershipReject(ctx, "AGENT_TOO_OLD", "Agent %s is too old to join running tasks; upgrade it first", label(node))
		}
		if joinSkipped && !sharedSkipCapable(caps) {
			return nil, membershipReject(ctx, "AGENT_UPGRADE_REQUIRED", sharedSkipUpgradeMessage, label(node))
		}
	}
	failoverReady := p.Failover != nil && p.Failover.Enabled
	for _, node := range p.ActiveNodes() {
		member, reachable := online[node.ID]
		// Without lease fencing (never enabled, including stopped tasks) an
		// offline member could still hold unfenced work, and a join could not
		// be admitted: every current member must be reachable.
		if !reachable || member.EnrollmentID != node.EnrollmentID {
			if failoverReady {
				continue // Offline members are fenced by lease expiry.
			}
			return nil, membershipReject(ctx, "AGENT_OFFLINE", "Agent %s is offline; reconnect it before changing this task's nodes", label(node))
		}
		caps, err := s.tasks.capabilities(ctx, node)
		if err != nil {
			if failoverReady {
				continue
			}
			return nil, membershipReject(ctx, "AGENT_VERSION_UNAVAILABLE", capabilitiesUnreadableMessage, label(node), err.Error())
		}
		if caps.LeaseProtocol < 1 || caps.QueuedHandoffProtocol < 1 {
			return nil, membershipReject(ctx, "AGENT_TOO_OLD", "Agent %s is too old for node changes; upgrade it first", label(node))
		}
	}
	var checks []bindingCheck
	if local && len(joining) > 0 {
		var failure *membershipFailure
		checks, failure = s.membershipPreflight(ctx, p, joining, online, displays, skip)
		if failure != nil {
			if request.DryRun {
				return s.membershipResult(ctx, p.ID, false, false, true, false, checks, []string{failure.message})
			}
			return nil, failure
		}
	}
	if request.DryRun {
		return s.membershipResult(ctx, p.ID, false, false, true, true, checks, nil)
	}
	if requestKey == "" {
		if requestKey, err = cluster.Token(); err != nil {
			return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot allocate request identity")}
		}
	}
	change.RequestKey = requestKey
	committed, replayed, failure := s.commitNodeChange(ctx, id, change, joining, label)
	if failure != nil {
		return nil, failure
	}
	// A cancelled join keeps no problem, even if the node is added again.
	s.tasks.joins.retain(committed)
	if skip && !replayed {
		added := make([]string, len(joining))
		for i, node := range joining {
			added[i] = fmt.Sprintf("%s at %q", label(node), node.LocalRoot)
		}
		log.Printf("task %d: node change %q (membership revision %d) adds %s without a shared directory check, at the operator's explicit request; these nodes were not verified to see the same storage as the task's other nodes", id, requestKey, committed.NodeRevision(), strings.Join(added, ", "))
	}
	select {
	case s.tasks.wake <- struct{}{}:
	default:
	}
	return s.membershipResult(ctx, id, true, replayed, false, true, checks, nil)
}

// commitNodeChange records a change while holding topologyMu, as
// createTransfer does: no Agent can be removed or enrolled anew in between,
// and each added Agent must still be online under the enrollment that the
// preflight used. removeAgentNode takes the same lock and then refuses to
// remove a member of an active task, which the added Agent now is. Under the
// lock this reads the online Agents (agentControl.mu) and writes the task
// (the store), the order removeAgentNode uses as well; the task runtime never
// takes topologyMu, so no cycle can form.
func (s *Server) commitNodeChange(ctx context.Context, id int64, change task.NodeChange, joining []task.Node, label func(task.Node) string) (task.Parent, bool, *membershipFailure) {
	s.topologyMu.Lock()
	defer s.topologyMu.Unlock()
	// A request accepted meanwhile replays: a retry is never refused because
	// an Agent it added went offline after joining.
	seen, err := s.tasks.repo.NodeRequestSeen(id, change.RequestKey, change.Fingerprint)
	if errors.Is(err, task.ErrConflict) {
		return task.Parent{}, false, membershipReject(ctx, "CONFLICT", "This request ID was already used for a different node change")
	}
	if err != nil {
		return task.Parent{}, false, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read task")}
	}
	if !seen && len(joining) > 0 {
		members, err := s.onlineAgents()
		if err != nil {
			return task.Parent{}, false, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read execution Agents")}
		}
		enrollments := make(map[string]string, len(members))
		for _, member := range members {
			enrollments[member.ID] = member.EnrollmentID
		}
		for _, node := range joining {
			if enrollment, ok := enrollments[node.ID]; !ok || enrollment != node.EnrollmentID {
				return task.Parent{}, false, membershipReject(ctx, "AGENT_OFFLINE", "Agent %s is not online in this enrollment", label(node))
			}
		}
	}
	if s.nodeChangeCommitting != nil {
		s.nodeChangeCommitting()
	}
	p, replayed, err := s.tasks.repo.ChangeNodes(id, change)
	if errors.Is(err, task.ErrConflict) {
		return task.Parent{}, false, membershipReject(ctx, "CONFLICT", "This request ID was already used for a different node change")
	}
	if err != nil {
		return task.Parent{}, false, s.membershipError(ctx, err)
	}
	return p, replayed, nil
}

func (s *Server) membershipError(ctx context.Context, err error) *membershipFailure {
	var rejection *task.MembershipError
	if errors.As(err, &rejection) {
		return &membershipFailure{status: 409, code: rejection.Code, message: membershipReason(ctx, err)}
	}
	if errors.Is(err, task.ErrNotFound) {
		return &membershipFailure{status: 404, message: localizeContext(ctx, "Task not found")}
	}
	return &membershipFailure{status: 400, message: localizeContext(ctx, err.Error())}
}

func (s *Server) membershipResult(ctx context.Context, id int64, accepted, replayed, dryRun, valid bool, checks []bindingCheck, problems []string) (map[string]any, *membershipFailure) {
	p, err := s.tasks.repo.Get(id)
	if err != nil {
		return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read task")}
	}
	view, err := s.taskNodesView(ctx, p)
	if err != nil {
		return nil, &membershipFailure{status: 500, message: localizeContext(ctx, "Cannot read execution Agents")}
	}
	if checks == nil {
		checks = []bindingCheck{}
	}
	if problems == nil {
		problems = []string{}
	}
	return map[string]any{"accepted": accepted, "dryRun": dryRun, "replayed": replayed, "valid": valid, "nodeBindings": checks, "validationErrors": problems, "view": view}, nil
}

// membershipPreflight runs the creation preflight for the added nodes: each
// added path must be an existing directory (and a download destination
// writable), then a marker written through one node must be read by every
// reachable current member and every added node (or, for an upload source no
// node can write, sampled files must match on all of them). Only a change
// that skips the check itself (skip) leaves the added nodes unverified; a
// skip recorded on the task by an earlier operator does not carry over, and
// the current members are never concerned by the skip.
func (s *Server) membershipPreflight(ctx context.Context, p task.Parent, joining []task.Node, online map[string]cluster.Member, displays nodeDisplaySet, skip bool) ([]bindingCheck, *membershipFailure) {
	spec := p.Spec
	spec.Nodes = nil
	spec.SharedCheckSkipped, spec.SharedCheckSkippedNodesOnly = skip, skip
	var checks []bindingCheck
	for _, node := range p.ActiveNodes() {
		if member, ok := online[node.ID]; ok && member.EnrollmentID == node.EnrollmentID {
			node.SharedCheckSkipped = false
			spec.Nodes = append(spec.Nodes, node)
			checks = append(checks, bindingCheck{taskBinding: taskBinding{node.Address, node.LocalRoot}, NodeDisplay: displays.lookup(node.ID, node.Address), Valid: true, PathType: "DIRECTORY"})
		}
	}
	if len(spec.Nodes) == 0 {
		return nil, membershipReject(ctx, "AGENT_OFFLINE", "Reconnect at least one current node to verify the shared directory")
	}
	existing := len(checks)
	var firstErr string
	for _, node := range joining {
		check := bindingCheck{taskBinding: taskBinding{node.Address, node.LocalRoot}, NodeDisplay: displays.lookup(node.ID, node.Address)}
		s.checkJoiningPath(ctx, spec.Direction, node, &check)
		if !check.Valid && firstErr == "" {
			firstErr = check.DisplayLabel + ": " + check.Message
		}
		checks = append(checks, check)
		node.SharedCheckSkipped = skip
		spec.Nodes = append(spec.Nodes, node)
	}
	if firstErr == "" {
		if err := s.validateSharedBindings(ctx, &spec, checks); err != nil {
			firstErr = err.Error()
		}
	} else {
		invalidateUnverifiedSharedBindings(ctx, checks)
	}
	added := checks[existing:]
	if firstErr != "" {
		// Report a failing current member too; it explains the added nodes' state.
		for _, check := range checks[:existing] {
			if !check.Valid && (check.Code == "SHARED_STORAGE_PROBE_FAILED" || check.Code == "SHARED_STORAGE_FINGERPRINT_FAILED" || check.Code == "AGENT_UPGRADE_REQUIRED") && check.Message != localizeContext(ctx, "Shared storage verification did not pass") {
				added = append(added, check)
			}
		}
		return added, &membershipFailure{status: 409, code: "PREFLIGHT_FAILED", message: firstErr, bindings: added}
	}
	return added, nil
}

// checkJoiningPath checks an added node's local path as the creation preflight
// checks a binding, with its messages and its write probe of a download
// destination (also when the shared check is skipped). The task's shared
// directory exists already and a join never creates it: a missing download
// destination is refused as missing before any write probe, which would
// otherwise probe (and ask to change the permissions of) a directory above
// it, and validateSharedBindings refuses any other path that is not a
// directory.
func (s *Server) checkJoiningPath(ctx context.Context, direction string, node task.Node, check *bindingCheck) {
	if !absoluteAgentPath(node.LocalRoot) || strings.ContainsRune(node.LocalRoot, 0) {
		check.Message = localizeContext(ctx, "本地路径必须为绝对路径")
		return
	}
	var probe struct {
		PathType string `json:"pathType"`
	}
	err := s.agentCall(ctx, nodeSession(node), "local_path", false, agentservice.PathRequest{Path: node.LocalRoot}, &probe)
	check.PathType = probe.PathType
	if err != nil {
		check.Message = err.Error()
		return
	}
	if message := localPathProblem(ctx, direction, probe.PathType); message != "" {
		check.Message = message
		return
	}
	if direction == "OSS_TO_LOCAL" && probe.PathType == "MISSING" {
		check.Code = "SHARED_DIRECTORY_REQUIRED"
		check.Message = localizeContext(ctx, "Shared destination directory does not exist")
		return
	}
	if message, writable := s.checkDownloadWritable(ctx, nodeSession(node), direction, node.LocalRoot, check); !writable {
		check.Code = "LOCAL_PATH_NOT_WRITABLE"
		check.Message = message
		return
	}
	check.Valid = true
	addFreeSpaceNote(ctx, check)
}

func digestValue(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
