package master

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

func setComparisonTiming(t *testing.T, value *time.Duration, d time.Duration) {
	t.Helper()
	previous := *value
	*value = d
	t.Cleanup(func() { *value = previous })
}

func comparisonGet(t *testing.T, f *agentHTTPFixture, method, path, language string, body any) (int, map[string]any) {
	t.Helper()
	code, _, result, err := localizedRequest(f.http.Client(), method, f.http.URL+path, f.admin, language, body)
	if err != nil {
		t.Fatal(err)
	}
	return code, result
}

func pageNumbers(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatal("page has no items array", body)
	}
	numbers := make([]int64, len(items))
	for i, item := range items {
		numbers[i] = int64(item.(map[string]any)["number"].(float64))
	}
	return numbers
}

func rowsQueries(t *testing.T, agent *fakeComparisonAgent) []comparison.RowsQuery {
	t.Helper()
	var queries []comparison.RowsQuery
	for _, payload := range agent.calledWith("comparison_rows") {
		var q comparison.RowsQuery
		if err := json.Unmarshal(payload, &q); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, q)
	}
	return queries
}

type legacyGetQuery struct {
	ID    string `json:"id"`
	After int64  `json:"after"`
	Limit int    `json:"limit"`
}

func legacyGetQueries(t *testing.T, agent *fakeComparisonAgent) []legacyGetQuery {
	t.Helper()
	var queries []legacyGetQuery
	for _, payload := range agent.calledWith("comparison_get") {
		var q legacyGetQuery
		if err := json.Unmarshal(payload, &q); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, q)
	}
	return queries
}

func TestComparisonPageValidatesParameters(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "page-params", agent)
	rows := fakeComparisonRows(3)
	agent.put(completedComparison("cmp-params", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-params"), time.Now())
	for _, test := range []struct{ query, message string }{
		{"after=-1", "Invalid comparison cursor"},
		{"after=x", "Invalid comparison cursor"},
		{"limit=0", "Comparison page size must be between 1 and 1000"},
		{"limit=1001", "Comparison page size must be between 1 and 1000"},
		{"limit=many", "Comparison page size must be between 1 and 1000"},
		{"status=MATCH", "Unknown comparison result status"},
		{"status=DIFFERENT,UNKNOWN", "Unknown comparison result status"},
	} {
		code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-params?"+test.query, "", nil)
		if code != 400 || body["message"] != test.message {
			t.Fatal(test.query, code, body)
		}
	}
	code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-params?status=EQUAL", "zh-CN", nil)
	if code != 400 || body["message"] != "比对结果状态无效" {
		t.Fatal("filter error was not localized", code, body)
	}
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/no-such-comparison", "", nil)
	if code != 404 || body["message"] != "Comparison not found" {
		t.Fatal("unknown comparison", code, body)
	}
	// The default page holds 50 rows; a status filter is case-insensitive and
	// sent in display order.
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-params?status=missing_target,%20different", "", nil)
	if code != 200 || !slices.Equal(pageNumbers(t, body), []int64{1, 3}) {
		t.Fatal(code, body)
	}
	queries := rowsQueries(t, agent)
	if len(queries) != 1 || queries[0].Limit != 50 || !slices.Equal(queries[0].Statuses, []string{comparison.StatusDifferent, comparison.StatusMissingTarget}) {
		t.Fatal("filter or default limit was not sent as given", queries)
	}
	if len(agent.calledWith("comparison_get")) != 0 {
		t.Fatal("a current Agent was asked for a legacy page")
	}
}

func TestComparisonPageContinuesFilteredRowsOnCurrentAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	agent.scanBudget = 100
	node := comparisonTestNode(t, f, "page-rows", agent)
	rows := fakeComparisonRows(1000)
	agent.put(completedComparison("cmp-rows", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-rows"), time.Now())
	// MISSING_TARGET rows are numbered 3, 10, 17, ...; each call examines at
	// most 100 rows, so 50 of them take four calls.
	want := make([]int64, 50)
	for i := range want {
		want[i] = int64(3 + 7*i)
	}
	code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-rows?status=MISSING_TARGET&limit=50", "", nil)
	if code != 200 || !slices.Equal(pageNumbers(t, body), want) || body["nextAfter"] != float64(346) || body["status"] != "COMPLETED" || body["missing"] != float64(143) {
		t.Fatal("filtered page lost or reordered rows", code, body)
	}
	queries := rowsQueries(t, agent)
	afters, limits := []int64{}, []int{}
	for _, q := range queries {
		afters, limits = append(afters, q.After), append(limits, q.Limit)
		if q.ID != "cmp-rows" || q.MaxBytes != comparison.MaxPageBytes || !slices.Equal(q.Statuses, []string{comparison.StatusMissingTarget}) {
			t.Fatal("rows query", q)
		}
	}
	if !slices.Equal(afters, []int64{0, 100, 200, 300}) || !slices.Equal(limits, []int{50, 36, 21, 7}) {
		t.Fatal("page did not continue from NextAfter with the remaining limit", afters, limits)
	}
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-rows?status=MISSING_TARGET&limit=2&after=346", "", nil)
	if code != 200 || !slices.Equal(pageNumbers(t, body), []int64{353, 360}) || body["nextAfter"] != float64(360) {
		t.Fatal("next page", code, body)
	}
	// After the time budget a short page returns where the Agent stopped.
	setComparisonTiming(t, &comparisonPageBudget, 0)
	agent.resetCalls()
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-rows?status=MISSING_TARGET&limit=50", "", nil)
	if code != 200 || len(pageNumbers(t, body)) != 14 || body["nextAfter"] != float64(100) || len(rowsQueries(t, agent)) != 1 {
		t.Fatal("budget did not end the page", code, body)
	}
	// Without a filter the last page ends without a cursor.
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-rows?after=990&limit=50", "", nil)
	if code != 200 || len(pageNumbers(t, body)) != 10 || body["nextAfter"] != nil {
		t.Fatal("last page", code, body)
	}
}

func TestComparisonPageFiltersLegacyRowsInMaster(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(false)
	node := comparisonTestNode(t, f, "page-legacy", agent)
	rows := fakeComparisonRows(450)
	p := completedComparison("cmp-legacy", rows)
	p.ResultsProtocol, p.ResultLimit, p.RowsTruncated = 0, comparison.LegacyStoredDifferences, true
	p.Error = "Comparison could not finish; check storage access and retry."
	agent.put(p, rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-legacy"), time.Now())
	// DIFFERENT rows are numbered 1, 8, 15, ...: the 30th is row 204, in the
	// middle of the second 200-row legacy page.
	want := make([]int64, 30)
	for i := range want {
		want[i] = int64(1 + 7*i)
	}
	code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-legacy?status=DIFFERENT&limit=30", "", nil)
	if code != 200 || !slices.Equal(pageNumbers(t, body), want) || body["nextAfter"] != float64(204) {
		t.Fatal("Master-side filter lost rows", code, body)
	}
	if body["truncatedReason"] != comparison.TruncatedLegacyLimit || body["rowsTruncated"] != true {
		t.Fatal("legacy truncation was not named", body)
	}
	queries := legacyGetQueries(t, agent)
	if len(queries) != 2 || queries[0] != (legacyGetQuery{"cmp-legacy", 0, 200}) || queries[1] != (legacyGetQuery{"cmp-legacy", 200, 200}) {
		t.Fatal("legacy pages", queries)
	}
	if len(agent.calledWith("comparison_rows")) != 0 {
		t.Fatal("an Agent without the capability was sent comparison_rows")
	}
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-legacy?status=DIFFERENT&limit=2&after=204", "", nil)
	if code != 200 || !slices.Equal(pageNumbers(t, body), []int64{211, 218}) || body["nextAfter"] != float64(218) {
		t.Fatal("continuation after a cut page", code, body)
	}
	// Unfiltered pages are read exactly, 200 rows at a time.
	agent.resetCalls()
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-legacy?limit=250", "", nil)
	numbers := pageNumbers(t, body)
	if code != 200 || len(numbers) != 250 || numbers[0] != 1 || numbers[249] != 250 || body["nextAfter"] != float64(250) {
		t.Fatal("unfiltered legacy page", code, body)
	}
	if queries := legacyGetQueries(t, agent); len(queries) != 2 || queries[0].Limit != 200 || queries[1] != (legacyGetQuery{"cmp-legacy", 200, 50}) {
		t.Fatal("unfiltered legacy requests", queries)
	}
	// Fixed sentences are localized; the specific detail is not.
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-legacy?status=ERROR&limit=1", "zh-CN", nil)
	item := body["items"].([]any)[0].(map[string]any)
	if code != 200 || item["error"] != "无法读取对象元数据" || item["errorDetail"] != "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)" || body["error"] != "比对未能完成，请检查存储访问后重试。" {
		t.Fatal("localization", code, body)
	}
}

func TestComparisonPageFallsBackWhenAgentRejectsRows(t *testing.T) {
	f := newAgentHTTPFixture(t)
	// The Agent advertises the protocol but answers like an older release,
	// as after a downgrade within the capability cache lifetime.
	agent := newFakeComparisonAgent(false)
	agent.advertised = comparison.Protocol
	node := comparisonTestNode(t, f, "page-downgraded", agent)
	rows := fakeComparisonRows(20)
	agent.put(completedComparison("cmp-downgraded", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-downgraded"), time.Now())
	for range 2 {
		agent.resetCalls()
		code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-downgraded?status=ERROR", "", nil)
		if code != 200 || !slices.Equal(pageNumbers(t, body), []int64{6, 13, 20}) {
			t.Fatal(code, body)
		}
		// The rejected capability is not remembered: each request asks again.
		if operations := agent.operations(); !slices.Equal(operations, []string{"capabilities", "comparison_rows", "comparison_get"}) {
			t.Fatal("fallback sequence", operations)
		}
	}
}

func TestComparisonProtocolCacheKeepsOnlyPositiveAnswers(t *testing.T) {
	var cache comparisonProtocols
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	queries, answer := 0, comparison.Protocol
	var failure error
	query := func(context.Context, agenttransport.Session) (int, error) { queries++; return answer, failure }
	node := agenttransport.Session{NodeID: "node", EnrollmentID: "first"}
	get := func(session agenttransport.Session) (int, error) {
		return cache.get(context.Background(), clock, session, query)
	}
	for range 2 {
		if protocol, err := get(node); protocol != comparison.Protocol || err != nil {
			t.Fatal(protocol, err)
		}
	}
	now = now.Add(comparisonProtocolTTL - time.Nanosecond)
	if _, err := get(node); err != nil || queries != 1 {
		t.Fatal("positive answer was not reused", queries, err)
	}
	if _, err := get(agenttransport.Session{NodeID: "node", EnrollmentID: "second"}); err != nil || queries != 2 {
		t.Fatal("a new enrollment reused the old answer", queries, err)
	}
	now = now.Add(time.Nanosecond)
	answer = 0
	for range 2 {
		if protocol, err := get(node); protocol != 0 || err != nil {
			t.Fatal(protocol, err)
		}
	}
	if queries != 4 {
		t.Fatal("an older answer was remembered", queries)
	}
	answer, failure = comparison.Protocol, errors.New("Agent is offline")
	if _, err := get(node); err == nil || queries != 5 {
		t.Fatal("failed query", queries, err)
	}
	failure = nil
	if protocol, err := get(node); protocol != comparison.Protocol || err != nil || queries != 6 {
		t.Fatal("a failure was remembered", queries, err)
	}
	cache.forget(node)
	if _, err := get(node); err != nil || queries != 7 {
		t.Fatal("forget kept the answer", queries, err)
	}
}

func createComparisonStorage(t *testing.T, f *agentHTTPFixture) storageconfig.Snapshot {
	t.Helper()
	snapshot, err := storageconfig.NewMaster(f.db).Create(storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "fixture-ak", SecretKey: "fixture-sk"})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestComparisonStartPrefersAgentsWithCurrentProtocol(t *testing.T) {
	f := newAgentHTTPFixture(t)
	createComparisonStorage(t, f)
	// Registry order is by node ID, so the older Agent comes first.
	legacy, current := newFakeComparisonAgent(false), newFakeComparisonAgent(true)
	legacyNode := comparisonTestNode(t, f, "a-legacy", legacy)
	currentNode := comparisonTestNode(t, f, "b-current", current)
	create := map[string]any{"sourceBucket": "qa/source", "destBucket": "qa/target", "sourcePrefix": "src/", "destPrefix": "dst/", "scope": "prefix", "mode": "metadata"}
	assigned := func(id string) comparisonAssignment {
		t.Helper()
		var assignment comparisonAssignment
		if err := f.db.View(func(tx *state.Tx) error {
			_, err := tx.Get(comparisonAssignmentsBucket, id, &assignment)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return assignment
	}
	assignments := func() int {
		t.Helper()
		count := 0
		if err := f.db.View(func(tx *state.Tx) error {
			return tx.Scan(comparisonAssignmentsBucket, "", "", 100, func(string, json.RawMessage) error { count++; return nil })
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	code, body := comparisonGet(t, f, "POST", "/api/master/comparisons", "", create)
	if code != 202 || body["status"] != "RUNNING" || body["resultsProtocol"] != float64(comparison.Protocol) {
		t.Fatal(code, body)
	}
	if assignment := assigned(body["id"].(string)); assignment.NodeID != currentNode.ID || time.Since(assignment.CreatedAt).Abs() > time.Hour {
		t.Fatal("comparison was not assigned to the current Agent with its creation time", assignment)
	}
	if len(legacy.calledWith("comparison_start")) != 0 {
		t.Fatal("older Agent was tried first")
	}
	// A busy current Agent leaves the older one as the fallback.
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons", "", create)
	if code != 202 || body["resultLimit"] != float64(comparison.LegacyStoredDifferences) {
		t.Fatal(code, body)
	}
	if assignment := assigned(body["id"].(string)); assignment.NodeID != legacyNode.ID || len(current.calledWith("comparison_start")) != 2 || assignments() != 2 {
		t.Fatal("busy Agent was not skipped", assignment)
	}
	// So does a current Agent that is being updated, which starts nothing.
	legacy.mu.Lock()
	for _, stored := range legacy.comparisons {
		stored.progress.Status = comparison.StateCompleted
	}
	legacy.mu.Unlock()
	current.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
		if c.Operation != "comparison_start" {
			return agenttransport.Response{}, false
		}
		return agenttransport.Response{Status: 409, Payload: json.RawMessage(`{"code":"updating","message":"Agent update is in progress"}`)}, true
	})
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons", "", create)
	if code != 202 {
		t.Fatal("updating Agent was not skipped", code, body)
	}
	if assignment := assigned(body["id"].(string)); assignment.NodeID != legacyNode.ID || len(current.calledWith("comparison_start")) != 3 || assignments() != 3 {
		t.Fatal("updating Agent kept an assignment for work it refused", assignment, assignments())
	}
}

func TestComparisonDeleteOfRunningComparisonConflicts(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current, legacy := newFakeComparisonAgent(true), newFakeComparisonAgent(false)
	currentNode := comparisonTestNode(t, f, "delete-current", current)
	legacyNode := comparisonTestNode(t, f, "delete-legacy", legacy)
	for _, target := range []struct {
		agent *fakeComparisonAgent
		node  string
	}{{current, "running-current"}, {legacy, "running-legacy"}} {
		p := completedComparison(target.node, nil)
		p.Status = comparison.StateRunning
		target.agent.put(p, nil)
	}
	putComparisonAssignment(t, f, currentNode, comparisonTestRequest("running-current"), time.Now())
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("running-legacy"), time.Now())
	code, body := comparisonGet(t, f, "DELETE", "/api/master/comparisons/running-current", "zh-CN", nil)
	if code != 409 || body["message"] != "比对正在运行，请先取消再删除" {
		t.Fatal(code, body)
	}
	// An older Agent reports its own running comparison as busy.
	code, body = comparisonGet(t, f, "DELETE", "/api/master/comparisons/running-legacy", "", nil)
	if code != 409 || body["message"] != "Comparison is running; cancel it before deleting" {
		t.Fatal(code, body)
	}
	if !comparisonAssignmentExists(t, f, "running-current") || !comparisonAssignmentExists(t, f, "running-legacy") {
		t.Fatal("a refused delete forgot the comparison")
	}
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/running-current/cancel", "", nil)
	if code != 200 || body["status"] != "CANCELED" {
		t.Fatal(code, body)
	}
	code, body = comparisonGet(t, f, "DELETE", "/api/master/comparisons/running-current", "", nil)
	if code != 200 || body["success"] != true || comparisonAssignmentExists(t, f, "running-current") {
		t.Fatal(code, body)
	}
	if _, found := current.progress("running-current"); found {
		t.Fatal("Agent kept the deleted comparison")
	}
	code, _ = comparisonGet(t, f, "DELETE", "/api/master/comparisons/running-current", "", nil)
	if code != 404 {
		t.Fatal("deleted comparison is still addressable", code)
	}
}

func TestComparisonResumeSyncsStorageAndMapsAgentReplies(t *testing.T) {
	f := newAgentHTTPFixture(t)
	snapshot := createComparisonStorage(t, f)
	current := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "resume-current", current)
	interrupted := completedComparison("cmp-resume", nil)
	interrupted.Status, interrupted.Resumable, interrupted.Checkpoint = comparison.StateInterrupted, true, "object/000100"
	interrupted.Error = "Comparison could not finish; check storage access and retry."
	current.put(interrupted, nil)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-resume"), time.Now())
	code, body := comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-resume/resume", "", nil)
	if code != 202 || body["status"] != "RUNNING" || body["resumes"] != float64(1) || body["checkpoint"] != "object/000100" {
		t.Fatal(code, body)
	}
	operations := current.operations()
	apply, resume := slices.Index(operations, "storage_apply"), slices.Index(operations, "comparison_resume")
	if apply < 0 || resume < apply {
		t.Fatal("storage was not synchronized before resuming", operations)
	}
	var sent comparison.ResumeRequest
	if err := json.Unmarshal(current.calledWith("comparison_resume")[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent != (comparison.ResumeRequest{ID: "cmp-resume", StorageRevision: snapshot.Revision}) {
		t.Fatal("resume request", sent)
	}
	// The Agent's own refusals keep their meaning.
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-resume/resume", "zh-CN", nil)
	if code != 409 || body["message"] != "该比对无法继续" {
		t.Fatal(code, body)
	}
	for _, test := range []struct {
		status          int
		message, expect string
	}{
		{429, "Another comparison is running on this node", "Another comparison is running on this node"},
		{409, storageconfig.ErrStaleRevision.Error(), "Storage connections changed; resume the comparison again"},
		{409, "Agent update is in progress", "Agent update is in progress"},
	} {
		current.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
			if c.Operation != "comparison_resume" {
				return agenttransport.Response{}, false
			}
			response, _ := fakeReply(test.status, map[string]string{"message": test.message})
			return response, true
		})
		code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-resume/resume", "", nil)
		if code != 409 || body["message"] != test.expect {
			t.Fatal(test, code, body)
		}
	}
	current.setIntercept(nil)
	current.mu.Lock()
	delete(current.comparisons, "cmp-resume")
	current.mu.Unlock()
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-resume/resume", "", nil)
	if code != 404 || body["message"] != "Comparison not found" || comparisonAssignmentExists(t, f, "cmp-resume") {
		t.Fatal("expired comparison", code, body)
	}
}

func TestComparisonResumeRequiresCurrentReachableAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	createComparisonStorage(t, f)
	legacy := newFakeComparisonAgent(false)
	downgraded := newFakeComparisonAgent(false)
	downgraded.advertised = comparison.Protocol
	legacyNode := comparisonTestNode(t, f, "resume-legacy", legacy)
	downgradedNode := comparisonTestNode(t, f, "resume-downgraded", downgraded)
	offline := offlineComparisonNode(t, f, "resume-offline")
	for _, node := range []struct {
		agent *fakeComparisonAgent
		id    string
	}{{legacy, "cmp-legacy"}, {downgraded, "cmp-downgraded"}} {
		p := completedComparison(node.id, nil)
		p.Status = comparison.StateCanceled
		node.agent.put(p, nil)
	}
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("cmp-legacy"), time.Now())
	putComparisonAssignment(t, f, downgradedNode, comparisonTestRequest("cmp-downgraded"), time.Now())
	putComparisonAssignment(t, f, offline, comparisonTestRequest("cmp-offline"), time.Now())
	code, body := comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-legacy/resume", "zh-CN", nil)
	if code != 409 || body["message"] != "请先升级该节点，再继续比对" {
		t.Fatal(code, body)
	}
	if operations := legacy.operations(); !slices.Equal(operations, []string{"capabilities"}) {
		t.Fatal("an older Agent was asked to resume", operations)
	}
	for range 2 {
		downgraded.resetCalls()
		code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-downgraded/resume", "", nil)
		if code != 409 || body["message"] != "Update this node to resume comparisons" {
			t.Fatal(code, body)
		}
		// The rejected operation drops the remembered capability.
		if operations := downgraded.operations(); !slices.Equal(operations, []string{"capabilities", "storage_apply", "comparison_resume"}) {
			t.Fatal(operations)
		}
	}
	code, body = comparisonGet(t, f, "POST", "/api/master/comparisons/cmp-offline/resume", "", nil)
	if code != 503 || body["message"] != "Agent is offline" || !comparisonAssignmentExists(t, f, "cmp-offline") {
		t.Fatal(code, body)
	}
	code, _ = comparisonGet(t, f, "POST", "/api/master/comparisons/unknown/resume", "", nil)
	if code != 404 {
		t.Fatal(code)
	}
}

func TestComparisonListReconcilesAssignmentsWithAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current, legacy := newFakeComparisonAgent(true), newFakeComparisonAgent(false)
	currentNode := comparisonTestNode(t, f, "list-current", current)
	legacyNode := comparisonTestNode(t, f, "list-legacy", legacy)
	offline := offlineComparisonNode(t, f, "list-offline")
	removed := offlineComparisonNode(t, f, "list-removed")
	for node, label := range map[string]string{currentNode.ID: "Current node", legacyNode.ID: "Legacy node", offline.ID: "Offline node"} {
		if _, err := f.master.agents.registry.UpdateLabel(node, label); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.master.agents.registry.Revoke(removed.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	at := func(hour int) time.Time { return time.Date(2026, 11, 1, hour, 0, 0, 0, time.UTC) }
	listed := completedComparison("current-listed", nil)
	listed.StartedAt = at(5)
	listed.Error = "Comparison could not finish; check storage access and retry."
	current.put(listed, nil)
	kept := completedComparison("legacy-kept", nil)
	kept.StartedAt, kept.RowsTruncated, kept.ResultsProtocol = at(3), true, 0
	legacy.put(kept, nil)
	// An Agent-side failure for one comparison leaves it UNAVAILABLE.
	legacy.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
		var q legacyGetQuery
		if c.Operation != "comparison_get" || json.Unmarshal(c.Payload, &q) != nil || q.ID != "legacy-failing" {
			return agenttransport.Response{}, false
		}
		response, _ := fakeReply(500, map[string]string{"code": "operation_failed", "message": "database is busy"})
		return response, true
	})
	putComparisonAssignment(t, f, currentNode, comparisonTestRequest("current-listed"), old)
	putComparisonAssignment(t, f, currentNode, comparisonTestRequest("current-gone"), old)
	putComparisonAssignment(t, f, currentNode, comparisonTestRequest("current-starting"), now)
	putComparisonAssignment(t, f, currentNode, comparisonTestRequest("current-unknown-age"), time.Time{})
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("legacy-kept"), old)
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("legacy-gone"), old)
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("legacy-starting"), now)
	putComparisonAssignment(t, f, legacyNode, comparisonTestRequest("legacy-failing"), at(1))
	putComparisonAssignment(t, f, offline, comparisonTestRequest("offline-a"), at(4))
	putComparisonAssignment(t, f, offline, comparisonTestRequest("offline-b"), time.Time{})
	putComparisonAssignment(t, f, removed, comparisonTestRequest("removed"), at(6))
	code, body := comparisonGet(t, f, "GET", "/api/master/comparisons", "zh-CN", nil)
	if code != 200 || body["complete"] != false {
		t.Fatal(code, body)
	}
	items := body["items"].([]any)
	var ids []string
	byID := map[string]map[string]any{}
	for _, raw := range items {
		item := raw.(map[string]any)
		ids = append(ids, item["id"].(string))
		byID[item["id"].(string)] = item
	}
	if !slices.Equal(ids, []string{"current-listed", "offline-a", "legacy-kept", "legacy-failing", "offline-b"}) {
		t.Fatal("list membership or order", ids)
	}
	if item := byID["current-listed"]; item["status"] != "COMPLETED" || item["nodeId"] != currentNode.ID || item["nodeName"] != "Current node (127.0.0.1)" || item["error"] != "比对未能完成，请检查存储访问后重试。" {
		t.Fatal("Agent item", item)
	}
	if item := byID["legacy-kept"]; item["truncatedReason"] != comparison.TruncatedLegacyLimit || item["nodeName"] != "Legacy node (127.0.0.1)" {
		t.Fatal("legacy item", item)
	}
	for _, id := range []string{"offline-a", "offline-b", "legacy-failing"} {
		item := byID[id]
		if item["status"] != "UNAVAILABLE" || item["mode"] != "content" || item["scope"] != "prefix" || item["sourcePrefix"] != "src/" || item["destinationPrefix"] != "dst/" || item["source"].(map[string]any)["bucket"] != "source" {
			t.Fatal("unavailable item", item)
		}
	}
	if byID["offline-a"]["startedAt"] != at(4).Format(time.RFC3339) || byID["offline-a"]["nodeName"] != "Offline node (127.0.0.1)" {
		t.Fatal("unavailable item lost its creation time or node", byID["offline-a"])
	}
	if _, present := byID["offline-b"]["startedAt"]; present {
		t.Fatal("unknown creation time was invented", byID["offline-b"])
	}
	unavailable, _ := json.Marshal(body["unavailableNodes"])
	want, _ := json.Marshal([]map[string]any{{"nodeId": legacyNode.ID, "nodeName": "Legacy node (127.0.0.1)", "count": 1}, {"nodeId": offline.ID, "nodeName": "Offline node (127.0.0.1)", "count": 2}})
	if string(unavailable) != string(want) {
		t.Fatal("unavailable nodes", string(unavailable))
	}
	for id, exists := range map[string]bool{"current-listed": true, "current-gone": false, "current-starting": true, "current-unknown-age": false, "legacy-kept": true, "legacy-gone": false, "legacy-starting": true, "legacy-failing": true, "offline-a": true, "offline-b": true, "removed": true} {
		if comparisonAssignmentExists(t, f, id) != exists {
			t.Fatal("reconciliation", id, exists)
		}
	}
	if code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/removed", "", nil); code != 503 || body["message"] != "The comparison node has been removed" {
		t.Fatal("comparison of a removed node", code, body)
	}
	if calls := current.calledWith("comparison_list"); len(calls) != 1 || string(calls[0]) != "{}" {
		t.Fatal("current Agent list calls", calls)
	}
	if len(current.calledWith("comparison_get")) != 0 || len(legacy.calledWith("comparison_get")) != 4 || len(legacy.calledWith("comparison_list")) != 0 {
		t.Fatal("protocol per node", current.operations(), legacy.operations())
	}
	// Once every node answers, the list is complete.
	legacy.setIntercept(nil)
	for _, id := range []string{"offline-a", "offline-b", "legacy-failing"} {
		if err := f.db.Update(func(tx *state.Tx) error { return tx.Delete(comparisonAssignmentsBucket, id) }); err != nil {
			t.Fatal(err)
		}
	}
	code, body = comparisonGet(t, f, "GET", "/api/master/comparisons", "", nil)
	if code != 200 || body["complete"] != true || len(body["unavailableNodes"].([]any)) != 0 || len(body["items"].([]any)) != 2 {
		t.Fatal(code, body)
	}
	if code, _ = comparisonGet(t, f, "GET", "/api/master/comparisons", "", nil); code != 200 {
		t.Fatal(code)
	}
	if code, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/comparisons", "", nil); code != 401 {
		t.Fatal("list did not require an administrator", code, body)
	}
}

// Every fixed sentence the comparison routes return, or relay from an Agent
// as a comparison or row error, has all three languages.
func TestComparisonMessagesAreLocalized(t *testing.T) {
	for _, message := range []string{
		"Cannot read storage connections", "Cannot create comparison", "Cannot read Agent state", "No connected Agent is available",
		"Cannot save comparison", "No Agent is available for comparison; a comparison may already be running", "Comparison not found",
		"The comparison node has been removed", "Cannot read comparison", "Cannot read comparisons", "Cannot read registered Agents",
		"Invalid comparison cursor", "Comparison page size must be between 1 and 1000", "Unknown comparison result status",
		"Comparison is running; cancel it before deleting", "Agent is offline", "Update this node to resume comparisons",
		"Another comparison is running on this node", "Storage connections changed; resume the comparison again",
		"Comparison cannot be resumed", "Agent update is in progress", "Agent returned an invalid comparison page",
		"Export format must be zip or jsonl", "Cannot create download ticket", "Too many unused download tickets",
		"Invalid download form", "Invalid or expired download ticket", "Cannot validate download session", "Sign in required",
		"invalid comparison request", "Specify a storage connection and bucket as alias/bucket", "Storage connection not found",
		"Comparison could not finish; check storage access and retry.", "Cannot save comparison results",
		"Comparison results are unavailable on this node", "Cannot read object metadata", "Cannot read source metadata",
		"Cannot read target metadata", "Cannot read complete object content",
	} {
		entry, ok := apiMessages[message]
		if !ok || entry.en == "" || entry.zh == "" || entry.ja == "" || entry.zh == entry.en || entry.ja == entry.en {
			t.Fatalf("%q is not localized: %+v", message, entry)
		}
	}
}
