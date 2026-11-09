package master

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func readComparisonAssignment(t *testing.T, f *agentHTTPFixture, id string) (comparisonAssignment, bool) {
	t.Helper()
	var a comparisonAssignment
	found := false
	if err := f.db.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get(comparisonAssignmentsBucket, id, &a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a, found
}

// listedComparisons returns the list items by ID and the reply.
func listedComparisons(t *testing.T, f *agentHTTPFixture) (map[string]map[string]any, map[string]any) {
	t.Helper()
	code, body := comparisonGet(t, f, "GET", "/api/master/comparisons", "", nil)
	if code != 200 {
		t.Fatal("list", code, body)
	}
	byID := map[string]map[string]any{}
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		byID[item["id"].(string)] = item
	}
	return byID, body
}

func TestComparisonIDsNeverStartWithDash(t *testing.T) {
	// About one random token in 64 starts with '-'; thousands of draws would
	// meet many of them.
	for range 5000 {
		id, err := comparisonID()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(id, "-") || len(id) != 43 {
			t.Fatalf("comparison ID %q", id)
		}
	}
}

func TestComparisonAgentNotFoundIsGoneOnlyForComparisonNotFound(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "rows-missing", agent)
	rows := fakeComparisonRows(3)
	agent.put(completedComparison("cmp-file", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-file"), time.Now())
	// An older Agent answers a missing result file with its own 404.
	agent.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
		if c.Operation != "comparison_rows" && c.Operation != "comparison_cancel" {
			return agenttransport.Response{}, false
		}
		response, _ := fakeReply(404, map[string]string{"message": "open /x/rows.jsonl: no such file or directory"})
		return response, true
	})
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/master/comparisons/cmp-file"},
		{"POST", "/api/master/comparisons/cmp-file/cancel"},
		{"GET", "/api/master/comparisons/cmp-file/export"},
	} {
		code, body := comparisonGet(t, f, route.method, route.path, "", nil)
		if code != 502 || !comparisonAssignmentExists(t, f, "cmp-file") {
			t.Fatal(route, code, body)
		}
	}
	agent.setIntercept(nil)
	if code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-file", "", nil); code != 200 || len(pageNumbers(t, body)) != 3 {
		t.Fatal("kept comparison", code, body)
	}
}

func TestComparisonOfRemovedNodeIsForgotten(t *testing.T) {
	f := newAgentHTTPFixture(t)
	removed := offlineComparisonNode(t, f, "removed-node")
	routes := []struct {
		method, path, id string
		code             int
	}{
		{"GET", "/api/master/comparisons/cmp-page", "cmp-page", 404},
		{"GET", "/api/master/comparisons/cmp-ticket/export-ticket", "cmp-ticket", 404},
		{"GET", "/api/master/comparisons/cmp-export/export", "cmp-export", 404},
		{"POST", "/api/master/comparisons/cmp-cancel/cancel", "cmp-cancel", 404},
		{"POST", "/api/master/comparisons/cmp-resume/resume", "cmp-resume", 404},
		{"DELETE", "/api/master/comparisons/cmp-delete", "cmp-delete", 200},
	}
	for _, route := range routes {
		putComparisonAssignment(t, f, removed, comparisonTestRequest(route.id), time.Now())
	}
	if _, err := f.master.agents.registry.Revoke(removed.ID); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		code, body := comparisonGet(t, f, route.method, route.path, "zh-CN", nil)
		if code != route.code || comparisonAssignmentExists(t, f, route.id) {
			t.Fatal(route, code, body)
		}
		if route.code == 404 && body["message"] != "比对记录不存在" || route.code == 200 && body["success"] != true {
			t.Fatal(route, code, body)
		}
	}
}

func TestComparisonRoutesAnswerAtOnceForOfflineNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	offline := offlineComparisonNode(t, f, "offline-node")
	putComparisonAssignment(t, f, offline, comparisonTestRequest("cmp-offline"), time.Now())
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/master/comparisons/cmp-offline?status=DIFFERENT"},
		{"POST", "/api/master/comparisons/cmp-offline/cancel"},
		{"DELETE", "/api/master/comparisons/cmp-offline"},
		{"GET", "/api/master/comparisons/cmp-offline/export-ticket"},
		{"GET", "/api/master/comparisons/cmp-offline/export?format=jsonl"},
		{"POST", "/api/master/comparisons/cmp-offline/resume"},
	} {
		code, body := comparisonGet(t, f, route.method, route.path, "zh-CN", nil)
		if code != 503 || body["message"] != "节点离线" || !comparisonAssignmentExists(t, f, "cmp-offline") {
			t.Fatal(route, code, body)
		}
	}
}

func TestComparisonAssignmentsFollowReenrolledNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	old := offlineComparisonNode(t, f, "reenrolled")
	for _, id := range []string{"cmp-listed", "cmp-running"} {
		putComparisonAssignment(t, f, old, comparisonTestRequest(id), time.Now())
	}
	if _, err := f.master.agents.registry.Revoke(old.ID); err != nil {
		t.Fatal(err)
	}
	// The node joins again with the same data directory, so the same node ID
	// and results, under a new enrollment.
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "reenrolled", agent)
	if node.ID != old.ID || node.EnrollmentID == old.EnrollmentID {
		t.Fatal("re-enrollment", old, node)
	}
	agent.put(completedComparison("cmp-listed", nil), nil)
	running := completedComparison("cmp-running", nil)
	running.Status, running.EndedAt = comparison.StateRunning, time.Time{}
	agent.put(running, nil)
	code, body := comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-running/cancel", "", nil)
	if code != 200 || body["status"] != comparison.StateCanceled {
		t.Fatal("cancel after re-enrollment", code, body)
	}
	if a, _ := readComparisonAssignment(t, f, "cmp-running"); a.NodeID != node.ID || a.EnrollmentID != node.EnrollmentID {
		t.Fatal("assignment was not rebound", a)
	}
	items, reply := listedComparisons(t, f)
	if reply["complete"] != true || len(items) != 2 || items["cmp-listed"]["status"] != comparison.StateCompleted || items["cmp-listed"]["nodeId"] != node.ID || items["cmp-running"]["status"] != comparison.StateCanceled {
		t.Fatal("list after re-enrollment", reply)
	}
	if a, _ := readComparisonAssignment(t, f, "cmp-listed"); a.EnrollmentID != node.EnrollmentID {
		t.Fatal("list did not rebind the assignment", a)
	}
}

func TestComparisonListAdoptsUnassignedAgentComparisons(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent, legacy := newFakeComparisonAgent(true), newFakeComparisonAgent(false)
	node := comparisonTestNode(t, f, "adopting", agent)
	comparisonTestNode(t, f, "adopting-legacy", legacy)
	// The Agent resumed a comparison the Master has no assignment for, for
	// instance after the Master database was restored from a backup.
	orphan := completedComparison("orphan", nil)
	orphan.Status, orphan.EndedAt = comparison.StateRunning, time.Time{}
	agent.put(orphan, nil)
	legacy.put(completedComparison("legacy-orphan", nil), nil)
	items, reply := listedComparisons(t, f)
	if reply["complete"] != true || len(items) != 1 || items["orphan"]["status"] != comparison.StateRunning || items["orphan"]["nodeId"] != node.ID {
		t.Fatal("unassigned comparison was not listed", reply)
	}
	a, found := readComparisonAssignment(t, f, "orphan")
	gotRequest, _ := json.Marshal(a.Request)
	wantRequest, _ := json.Marshal(orphan.Request)
	if !found || a.NodeID != node.ID || a.EnrollmentID != node.EnrollmentID || !a.CreatedAt.Equal(orphan.StartedAt) || string(gotRequest) != string(wantRequest) {
		t.Fatal("adopted assignment", found, a)
	}
	if operations := legacy.operations(); slices.Contains(operations, "comparison_get") || slices.Contains(operations, "comparison_list") {
		t.Fatal("an Agent without assignments was asked for comparisons it cannot list", operations)
	}
	code, body := comparisonGet(t, f, "POST", "/api/master/comparisons/orphan/cancel", "", nil)
	if code != 200 || body["status"] != comparison.StateCanceled || len(agent.calledWith("comparison_cancel")) != 1 {
		t.Fatal("cancel of an adopted comparison", code, body)
	}
	code, body = comparisonGet(t, f, "DELETE", "/api/master/comparisons/orphan", "", nil)
	if code != 200 || comparisonAssignmentExists(t, f, "orphan") {
		t.Fatal("delete of an adopted comparison", code, body)
	}
	if _, found := agent.progress("orphan"); found {
		t.Fatal("Agent kept the deleted comparison")
	}
	if items, _ := listedComparisons(t, f); len(items) != 0 {
		t.Fatal("deleted comparison is listed", items)
	}
}

func TestComparisonListKeepsAssignmentsWhenAgentListFails(t *testing.T) {
	for _, operation := range []string{"comparison_list", "capabilities"} {
		t.Run(operation, func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			agent := newFakeComparisonAgent(true)
			node := comparisonTestNode(t, f, "list-failing", agent)
			agent.put(completedComparison("cmp-old", nil), nil)
			putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-old"), time.Now().Add(-time.Hour))
			putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-young"), time.Now())
			putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-unknown-age"), time.Time{})
			agent.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
				if c.Operation != operation {
					return agenttransport.Response{}, false
				}
				response, _ := fakeReply(500, map[string]string{"code": "operation_failed", "message": "database is busy"})
				return response, true
			})
			items, reply := listedComparisons(t, f)
			unavailable := reply["unavailableNodes"].([]any)
			if reply["complete"] != false || len(items) != 3 || len(unavailable) != 1 || unavailable[0].(map[string]any)["nodeId"] != node.ID || unavailable[0].(map[string]any)["count"] != float64(3) {
				t.Fatal("failing Agent", reply)
			}
			for _, id := range []string{"cmp-old", "cmp-young", "cmp-unknown-age"} {
				if items[id]["status"] != comparisonUnavailable || !comparisonAssignmentExists(t, f, id) {
					t.Fatal(id, items[id])
				}
			}
		})
	}
}

func TestComparisonListFallsBackWhenAgentRejectsList(t *testing.T) {
	f := newAgentHTTPFixture(t)
	// The Agent advertises the protocol but answers like an older release.
	agent := newFakeComparisonAgent(false)
	agent.advertised = comparison.Protocol
	node := comparisonTestNode(t, f, "list-downgraded", agent)
	agent.put(completedComparison("cmp-listed", nil), nil)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-listed"), time.Now().Add(-time.Hour))
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-gone"), time.Now().Add(-time.Hour))
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-starting"), time.Now())
	items, reply := listedComparisons(t, f)
	if reply["complete"] != true || len(items) != 1 || items["cmp-listed"]["status"] != comparison.StateCompleted {
		t.Fatal("fallback list", reply)
	}
	if operations := agent.operations(); !slices.Equal(operations, []string{"capabilities", "comparison_list", "comparison_get", "comparison_get", "comparison_get"}) {
		t.Fatal("fallback sequence", operations)
	}
	for id, exists := range map[string]bool{"cmp-listed": true, "cmp-gone": false, "cmp-starting": true} {
		if comparisonAssignmentExists(t, f, id) != exists {
			t.Fatal("reconciliation", id, exists)
		}
	}
	// The rejected capability is not remembered.
	agent.resetCalls()
	listedComparisons(t, f)
	if operations := agent.operations(); len(operations) == 0 || operations[0] != "capabilities" {
		t.Fatal("capability was kept after the rejection", operations)
	}
}
