package master

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// A node added to a task joins only through the rejoin handshake
// (rejoinAvailableNodes), which runs once takeover is set up for the task and
// is repeated on every scheduler step until it succeeds. Each step records,
// for every joining node, why the handshake has not finished, so that the
// node view can say why a node stays joining (and blocks later node changes)
// and the log names each reason once. A reason is kept as a code with
// arguments and localized when the view is read. The record lives in memory:
// a restarted Master rebuilds it within one step.

// Join problem codes. The first verb of each message (joinProblemMessages)
// names the joining node; the problem's arguments fill the others.
const (
	joinAgentOffline       = "AGENT_OFFLINE"
	joinEnrollmentChanged  = "ENROLLMENT_CHANGED"
	joinMaintenance        = "AGENT_MAINTENANCE"
	joinMaintenanceUnknown = "MAINTENANCE_UNKNOWN"
	joinScanUnfinished     = "SCAN_UNFINISHED"
	joinVersionUnavailable = "AGENT_VERSION_UNAVAILABLE"
	joinAgentTooOld        = "AGENT_TOO_OLD"
	joinUpgradeRequired    = "AGENT_UPGRADE_REQUIRED"
	joinStorageUnverified  = "SHARED_STORAGE_UNVERIFIED"
	joinLeaseFailed        = "LEASE_REJOIN_FAILED"
	joinNotRecorded        = "JOIN_NOT_RECORDED"
	joinExhausted          = "REVISIONS_EXHAUSTED"
	joinTakeoverInProgress = "TAKEOVER_IN_PROGRESS"
	joinScanHandoff        = "SCAN_HANDOFF"
	joinTakeoverWaiting    = "TAKEOVER_WAITING_FOR_NODES"
	joinTakeoverNotReady   = "TAKEOVER_NOT_READY"
	joinTaskProblem        = "TASK_PROBLEM"
	// joinTaskNotRunning is derived when the view is read: no step runs for a
	// task that is not running, so nothing is recorded for it.
	joinTaskNotRunning = "TASK_NOT_RUNNING"
)

// joinProblemMessages are the source texts, translated in i18n_join_problems.go.
var joinProblemMessages = map[string]string{
	joinAgentOffline:       "Agent %s is offline; it joins after it reconnects",
	joinEnrollmentChanged:  "Agent %s was enrolled again after it was added to this task and cannot join with its earlier enrollment. Cancel the join, then add the node again",
	joinMaintenance:        "Agent %s is in an upgrade maintenance window; it joins after the window ends",
	joinMaintenanceUnknown: "Cannot read whether Agent %s is in an upgrade maintenance window: %s",
	joinScanUnfinished:     "Agent %s was the scanning node of this task; it joins after another node has taken over the scan",
	joinVersionUnavailable: capabilitiesUnreadableMessage,
	joinAgentTooOld:        "Agent %s is too old to join running tasks; upgrade it first",
	joinUpgradeRequired:    sharedSkipUpgradeMessage,
	joinStorageUnverified:  "Agent %s has not passed the shared directory check: %s",
	joinLeaseFailed:        "The execution lease of Agent %s for this task could not be set up: %s",
	joinNotRecorded:        "Cannot record the join of Agent %s: %s",
	joinExhausted:          "Agent %s cannot join: this task has used up the revision numbers that a join needs. Cancel the join",
	joinTakeoverInProgress: "Agent %s joins after the work of a lost or removed node has moved to other nodes",
	joinScanHandoff:        "Agent %s joins after the scan has moved to another node",
	joinTakeoverWaiting:    "Agent %s joins after takeover is set up for this task. Setting it up waits for these current nodes to be available: %s",
	joinTakeoverNotReady:   "Agent %s joins after takeover is set up for this task. Setting it up needs every current node to report that it supports takeover",
	joinTaskProblem:        "Agent %s joins after the task's current problem is resolved: %s",
	joinTaskNotRunning:     "The task is not running; Agent %s joins after the task runs again",
}

// joinProblem is why a joining node has not joined yet.
type joinProblem struct {
	Code string
	// Args fill the message's verbs after the first, which names the node.
	Args []any
}

func newJoinProblem(code string, args ...any) joinProblem {
	return joinProblem{Code: code, Args: args}
}

// message is the problem in the request's language (the source text without
// one), with node naming the joining node.
func (problem joinProblem) message(ctx context.Context, node string) string {
	return fmt.Sprintf(localizeContext(ctx, joinProblemMessages[problem.Code]), append([]any{node}, problem.Args...)...)
}

// maxLoggedJoinProblems bounds the distinct problems logged for one join, as
// a problem that carries changing details could otherwise log every step.
const maxLoggedJoinProblems = 16

// joinRecord is the latest problem of one join attempt of a node.
type joinRecord struct {
	// attempt is when the join was requested (MembershipState.Joining): a
	// node that is removed and added again starts a new attempt.
	attempt time.Time
	problem joinProblem
	// logged holds the problems of this attempt already logged, as English text.
	logged map[string]bool
	muted  bool
}

// joinProblems holds the latest problem of each joining node, per task.
type joinProblems struct {
	mu    sync.Mutex
	tasks map[int64]map[string]*joinRecord
}

// joinLog says what to log for a noted problem.
type joinLog int

const (
	joinLogNothing joinLog = iota
	joinLogProblem         // the problem is new to this join attempt
	joinLogMuted           // too many distinct problems; later ones are not logged
)

func joinAttempt(p task.Parent, nodeID string) (time.Time, bool) {
	if p.Membership == nil {
		return time.Time{}, false
	}
	attempt, joining := p.Membership.Joining[nodeID]
	return attempt, joining
}

// note records problem as the latest of a node that joins p.
func (j *joinProblems) note(p task.Parent, nodeID string, problem joinProblem) joinLog {
	attempt, joining := joinAttempt(p, nodeID)
	if !joining {
		return joinLogNothing
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.tasks == nil {
		j.tasks = map[int64]map[string]*joinRecord{}
	}
	records := j.tasks[p.ID]
	if records == nil {
		records = map[string]*joinRecord{}
		j.tasks[p.ID] = records
	}
	record := records[nodeID]
	if record == nil || !record.attempt.Equal(attempt) {
		record = &joinRecord{attempt: attempt, logged: map[string]bool{}}
		records[nodeID] = record
	}
	record.problem = problem
	text := problem.message(context.Background(), nodeID)
	switch {
	case record.logged[text] || record.muted:
		return joinLogNothing
	case len(record.logged) == maxLoggedJoinProblems:
		record.muted = true
		return joinLogMuted
	}
	record.logged[text] = true
	return joinLogProblem
}

// latest returns the problem last recorded for the current join of a node.
func (j *joinProblems) latest(p task.Parent, nodeID string) (joinProblem, bool) {
	attempt, joining := joinAttempt(p, nodeID)
	if !joining {
		return joinProblem{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	record := j.tasks[p.ID][nodeID]
	if record == nil || !record.attempt.Equal(attempt) {
		return joinProblem{}, false
	}
	return record.problem, true
}

// clear forgets a node's problem once it joined. It reports whether a
// problem of the node had been logged.
func (j *joinProblems) clear(id int64, nodeID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	record := j.tasks[id][nodeID]
	if record == nil {
		return false
	}
	delete(j.tasks[id], nodeID)
	if len(j.tasks[id]) == 0 {
		delete(j.tasks, id)
	}
	return len(record.logged) > 0
}

// retain forgets the problems of nodes that no longer join p: they joined,
// or their join was cancelled (perhaps to be added again, a new attempt).
func (j *joinProblems) retain(p task.Parent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	records := j.tasks[p.ID]
	for nodeID, record := range records {
		if attempt, joining := joinAttempt(p, nodeID); !joining || !record.attempt.Equal(attempt) {
			delete(records, nodeID)
		}
	}
	if len(records) == 0 {
		delete(j.tasks, p.ID)
	}
}

// forget drops a task's problems: it was deleted, or it ended or stopped, so
// that no step runs for it until it is resumed.
func (j *joinProblems) forget(id int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.tasks, id)
}

// prune keeps only the problems of the given (active) tasks.
func (j *joinProblems) prune(active map[int64]bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id := range j.tasks {
		if !active[id] {
			delete(j.tasks, id)
		}
	}
}

// noteJoinProblem records why node, which joins p, has not joined yet, and
// logs each distinct problem once per join attempt.
func (t *taskRuntime) noteJoinProblem(p task.Parent, node task.Node, label string, problem joinProblem) {
	switch t.joins.note(p, node.ID, problem) {
	case joinLogProblem:
		log.Printf("task %d generation %d: a joining node has not joined yet: %s", p.ID, p.Generation, problem.message(context.Background(), label))
	case joinLogMuted:
		log.Printf("task %d generation %d: node %s has not joined yet for more reasons than are logged; the task's node view shows the latest", p.ID, p.Generation, label)
	}
}

// noteJoinsWaiting records the same problem for every node that joins p.
func (t *taskRuntime) noteJoinsWaiting(p task.Parent, problem joinProblem) {
	if p.Membership == nil || len(p.Membership.Joining) == 0 {
		return
	}
	displays := t.server.nodeDisplaySnapshot()
	for _, node := range p.Spec.Nodes {
		if p.NodeJoining(node.ID) {
			t.noteJoinProblem(p, node, displays.lookup(node.ID, node.Address).DisplayLabel, problem)
		}
	}
}

// noteJoinsBlocked records why no node could join in a step that did not
// reach the rejoin handshake: takeover is not set up yet, or the takeover
// reconciliation that precedes the handshake failed.
func (t *taskRuntime) noteJoinsBlocked(ctx context.Context, p task.Parent, available map[string]bool, err error) {
	if p.Membership == nil || len(p.Membership.Joining) == 0 || ctx.Err() != nil {
		return
	}
	var problem joinProblem
	switch {
	case err != nil:
		message := runtimeProblemMessage(withoutPlannedProblems(p, err))
		if message == "" {
			return
		}
		problem = newJoinProblem(joinTaskProblem, strings.ReplaceAll(message, "\n", "; "))
	case p.Failover != nil && p.Failover.Enabled:
		return
	default:
		var waiting []task.Node
		for _, node := range p.ActiveNodes() {
			if !available[node.ID] {
				waiting = append(waiting, node)
			}
		}
		problem = newJoinProblem(joinTakeoverNotReady)
		if len(waiting) > 0 {
			problem = newJoinProblem(joinTakeoverWaiting, t.server.nodeLabels(waiting))
		}
	}
	t.noteJoinsWaiting(p, problem)
}

// joinedNode forgets a node's problem once it has joined, and logs the join
// when a problem of it had been logged.
func (t *taskRuntime) joinedNode(p task.Parent, label, nodeID string) {
	if t.joins.clear(p.ID, nodeID) {
		log.Printf("task %d generation %d: node %s has joined", p.ID, p.Generation, label)
	}
}

// addJoinProblems says, in the request's language, why each joining node in
// the node view's rows has not joined yet.
func (s *Server) addJoinProblems(ctx context.Context, p task.Parent, displays nodeDisplaySet, rows []map[string]any) {
	running := p.Status == "RUNNING" || p.Status == "RESUMING"
	for _, row := range rows {
		if row["state"] != "JOINING" {
			continue
		}
		id, _ := row["id"].(string)
		address, _ := row["address"].(string)
		problem, found := newJoinProblem(joinTaskNotRunning), true
		if running {
			problem, found = s.tasks.joins.latest(p, id)
		}
		if found {
			row["joinProblem"] = map[string]any{"code": problem.Code, "message": problem.message(ctx, displays.lookup(id, address).DisplayLabel)}
		}
	}
}
