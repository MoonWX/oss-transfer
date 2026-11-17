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
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
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
