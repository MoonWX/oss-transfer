package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// capturedLog holds what the standard logger writes during a test.
type capturedLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.Write(p)
}

// matching returns the lines that contain every part.
func (c *capturedLog) matching(parts ...string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(c.buffer.String(), "\n") {
		found := line != ""
		for _, part := range parts {
			found = found && strings.Contains(line, part)
		}
		if found {
			lines = append(lines, line)
		}
	}
	return lines
}

func captureLog(t *testing.T) *capturedLog {
	t.Helper()
	captured := &capturedLog{}
	previous := log.Writer()
	log.SetOutput(captured)
	t.Cleanup(func() { log.SetOutput(previous) })
	return captured
}

// Granting a skipped check is logged when an Agent process first receives it,
// not on every scheduler step. Creating a task or changing its nodes with the
// check skipped is logged when it happens, not when a repeated request
// returns its result.
func TestSkippedCheckLogsRecordEventsOnce(t *testing.T) {
	f := newAgentHTTPFixture(t)
	first, second := t.TempDir(), t.TempDir()
	p, agents := sharedSourceParent(t, f, first, second)
	p.Spec.SharedCheckSkipped = true
	logs := captureLog(t)
	ctx := context.Background()
	for range 5 {
		if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, false); err != nil {
			t.Fatal(err)
		}
		if err := f.master.tasks.ensureSharedSource(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{first, second} {
		if lines := logs.matching(fmt.Sprintf("task %d generation %d", p.ID, p.Generation), "works in "+root+" without a shared directory check"); len(lines) != 1 {
			t.Fatal("a grant was not logged exactly once", root, lines)
		}
	}

	createComparisonStorage(t, f)
	row := uploadRow(taskBinding{agents[0].node.Address, first}, taskBinding{agents[1].node.Address, first})
	row.SkipSharedCheck, row.SkipSharedCheckAcknowledged = true, true
	encoded, err := json.Marshal([]taskCreateRequest{row})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err := http.NewRequest("POST", f.http.URL+"/api/master/local-transfers", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "create-once")
		response, err := f.http.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || result["data"].(map[string]any)["acceptedCount"] != float64(1) {
			t.Fatal("the skipped task was not created", response.StatusCode, result, err)
		}
	}
	if lines := logs.matching("created with the shared directory check skipped"); len(lines) != 1 {
		t.Fatal("the creation was not logged exactly once", lines)
	}

	// A node change that skips the check is logged once, with the task, the
	// added node and its path, the request and the membership revision.
	joiner := overrideTestAgent(t, f, 3, func(agenttransport.Command) (agenttransport.Response, bool) { return agenttransport.Response{}, false })
	q := localTask(t, f, "audited", "LOCAL_TO_OSS", first, false, agents[0].node, agents[1].node)
	change := joinBody(joiner, second, acknowledgedSkip, map[string]any{"requestId": "join-unchecked"})
	for attempt := range 2 {
		status, body := nodeChange(t, f, q.ID, "", change)
		if data, _ := body["data"].(map[string]any); status != 200 || data["accepted"] != true || data["replayed"] != (attempt == 1) {
			t.Fatal("the change was not accepted, then replayed", attempt, status, body)
		}
	}
	lines := logs.matching(fmt.Sprintf("task %d: node change %q (membership revision 1)", q.ID, "join-unchecked"), fmt.Sprintf("%q", second), "without a shared directory check")
	if len(lines) != 1 {
		t.Fatal("the change was not logged exactly once", lines)
	}
}

// A lost node's work that moves without a shared directory check is logged
// once, when the node is retired: not on a step in which its retirement fails
// or is held back by an upgrade window that has just begun, and not again on
// the steps after it. Only the nodes whose check was skipped are named as
// taking the work over without one.
func TestUncheckedTakeoverIsLoggedOnceTheNodeIsRetired(t *testing.T) {
	f := newAgentHTTPFixture(t)
	start := time.Now()
	var late, lost atomic.Bool
	// The survivor's handler runs act on its countdown-th check grant of a
	// step: the second one, which comes between the check that B's lease has
	// expired and B's retirement.
	var countdown atomic.Int64
	var act atomic.Pointer[func()]
	a := overrideTestAgent(t, f, 1, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "shared_proof_skip" && countdown.Add(-1) == 0 {
			(*act.Load())()
		}
		return agenttransport.Response{}, false
	})
	b := overrideTestAgent(t, f, 2, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if lost.Load() && command.Operation == "capabilities" {
			return agentReply(503, map[string]string{"code": "unavailable", "message": "the node is lost"})
		}
		return agenttransport.Response{}, false
	})
	labelAgent(t, f, a, "Survivor A")
	labelAgent(t, f, b, "Lost B")
	p := localTask(t, f, "lost-unchecked", "LOCAL_TO_OSS", t.TempDir(), true, a, b)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	// The lease clock reads late, when every lease has expired, while late
	// is set.
	runtime.leases.now = func() time.Time {
		if late.Load() {
			return start.Add(time.Hour)
		}
		return start
	}
	logs := captureLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, err := runtime.reconcileFailover(ctx, p, map[string]bool{a.ID: true, b.ID: true})
	if err != nil || p.Failover == nil || !p.Failover.Enabled {
		t.Fatal("takeover was not set up", err)
	}
	lost.Store(true)
	// step runs a scheduler step without B, with act at the second grant.
	step := func(action func()) error {
		t.Helper()
		act.Store(&action)
		countdown.Store(2)
		late.Store(true)
		next, err := runtime.reconcileFailover(ctx, p, map[string]bool{a.ID: true})
		if current, readErr := runtime.repo.Get(p.ID); readErr == nil {
			p = current
		} else {
			p = next
		}
		return err
	}
	isLost := func() []string {
		return logs.matching(fmt.Sprintf("task %d generation %d: node Lost B (127.0.0.1) is lost; its work moves to Survivor A (127.0.0.1) without a shared directory check", p.ID, p.Generation))
	}
	maintenance := func(expires time.Time) func() {
		return func() {
			window := agentMaintenance{ID: "upgrade", EnrollmentID: b.EnrollmentID, ExpiresAt: expires}
			if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("agent-maintenance", b.ID, window) }); err != nil {
				t.Error(err)
			}
		}
	}

	// B's lease is found expired, but the retirement finds it not yet so.
	if err := step(func() { late.Store(false) }); err == nil || !strings.Contains(err.Error(), "waiting for the previous node's task lease to expire") || p.NodeRetired(b.ID) {
		t.Fatal("the retirement did not fail as the test intends", err, p.Failover)
	}
	// An upgrade window of B begins just before the retirement, which then
	// keeps B without an error.
	if err := step(maintenance(f.master.options.Now().Add(time.Hour))); err != nil || p.NodeRetired(b.ID) {
		t.Fatal("the upgrade window did not hold the retirement back", err, p.Failover)
	}
	if lines := isLost(); len(lines) != 0 {
		t.Fatal("a node that was not retired was logged as lost", lines)
	}
	maintenance(f.master.options.Now().Add(-time.Second))()
	for range 3 {
		if err := step(func() {}); err != nil {
			t.Fatal(err)
		}
	}
	if !p.NodeRetired(b.ID) {
		t.Fatal("the lost node was not retired", p.Failover)
	}
	if lines := isLost(); len(lines) != 1 {
		t.Fatal("the retirement was not logged exactly once", lines)
	}

	// Where only some nodes skipped the check, only they are named so.
	nodesOnly := p
	nodesOnly.Spec.SharedCheckSkippedNodesOnly = true
	unchecked := task.Node{ID: "unchecked", EnrollmentID: "unchecked-enrollment", Address: "unchecked.example", SharedCheckSkipped: true}
	nodesOnly.Spec.Nodes = append(append([]task.Node(nil), p.Spec.Nodes...), unchecked)
	runtime.logUncheckedTakeover(nodesOnly, b, []task.Node{a, unchecked})
	runtime.logUncheckedTakeover(nodesOnly, b, []task.Node{a})
	if lines := logs.matching("is lost; its work moves to Survivor A (127.0.0.1), and to Node ID: unchecked.example without a shared directory check"); len(lines) != 1 {
		t.Fatal("the unchecked node was not named alone", logs.matching("is lost"))
	}
	if lines := logs.matching("is lost"); len(lines) != 2 {
		t.Fatal("work that moves to verified nodes only was logged", lines)
	}
}
