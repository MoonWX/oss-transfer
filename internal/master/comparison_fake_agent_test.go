package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// fakeComparisonAgent is an in-memory Agent for the comparison operations. A
// modern one advertises comparison.Protocol and serves comparison_rows,
// comparison_list and comparison_resume as the contract describes; a legacy
// one has only comparison_start/get/cancel/delete with the replies of
// releases before Protocol 2. It runs behind the real reverse transport.
type fakeComparisonAgent struct {
	mu sync.Mutex
	// advertised is the comparisonProtocol capability; 0 omits it.
	advertised int
	// modern serves the operations of comparison.Protocol.
	modern bool
	// scanBudget is how many rows one comparison_rows call examines (0: all).
	scanBudget  int
	revision    uint64
	comparisons map[string]*fakeComparison
	calls       []fakeComparisonCall
	// intercept, when set, may answer a command before the fake does.
	intercept func(agenttransport.Command) (agenttransport.Response, bool)
}

type fakeComparison struct {
	progress comparison.Progress
	rows     []comparison.Row
}

type fakeComparisonCall struct {
	operation string
	payload   json.RawMessage
}

func newFakeComparisonAgent(modern bool) *fakeComparisonAgent {
	a := &fakeComparisonAgent{modern: modern, comparisons: map[string]*fakeComparison{}}
	if modern {
		a.advertised = comparison.Protocol
	}
	return a
}

// ReplaySafe lets the fake run mutating commands without an idempotency
// journal, as the real Agent does for its comparison operations.
func (a *fakeComparisonAgent) ReplaySafe(string) bool { return true }

func fakeReply(status int, value any) (agenttransport.Response, error) {
	data, err := json.Marshal(value)
	return agenttransport.Response{Status: status, Payload: data}, err
}

func fakeUnknownOperation() (agenttransport.Response, error) {
	return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
}

func fakeDecode(data json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func (a *fakeComparisonAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	a.mu.Lock()
	a.calls = append(a.calls, fakeComparisonCall{c.Operation, append(json.RawMessage(nil), c.Payload...)})
	intercept := a.intercept
	a.mu.Unlock()
	if intercept != nil {
		if response, handled := intercept(c); handled {
			return response, nil
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	notFound := func() (agenttransport.Response, error) {
		return fakeReply(404, map[string]string{"message": "Comparison not found"})
	}
	invalid := func(err error) (agenttransport.Response, error) {
		return fakeReply(400, map[string]string{"code": "invalid_request", "message": err.Error()})
	}
	switch c.Operation {
	case "capabilities":
		caps := map[string]any{"leaseProtocol": 2}
		if a.advertised > 0 {
			caps["comparisonProtocol"] = a.advertised
		}
		return fakeReply(200, caps)
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := json.Unmarshal(c.Payload, &snapshot); err != nil {
			return invalid(err)
		}
		a.revision = snapshot.Revision
		return fakeReply(200, map[string]any{"revision": snapshot.Revision})
	case "comparison_start":
		var r comparison.Request
		if err := fakeDecode(c.Payload, &r); err != nil {
			return invalid(err)
		}
		for _, existing := range a.comparisons {
			if existing.progress.Status == comparison.StateRunning {
				return fakeReply(429, map[string]string{"message": "Another comparison is running on this node"})
			}
		}
		now := time.Now().UTC()
		p := comparison.Progress{Request: r, Status: comparison.StateRunning, StartedAt: now, UpdatedAt: now}
		if a.modern {
			p.ResultsProtocol = comparison.Protocol
		} else {
			p.ResultLimit = comparison.LegacyStoredDifferences
		}
		a.comparisons[r.ID] = &fakeComparison{progress: p}
		return fakeReply(200, p)
	case "comparison_get":
		var q struct {
			ID    string `json:"id"`
			After int64  `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		if q.After < 0 || q.Limit < 1 || q.Limit > comparison.MaxLegacyPageRows {
			// Releases before Protocol 2 reply to bad paging with HTTP 500.
			return fakeReply(500, map[string]string{"code": "operation_failed", "message": "invalid result pagination"})
		}
		stored := a.comparisons[q.ID]
		if stored == nil {
			return notFound()
		}
		page := comparison.Page{Progress: stored.progress, Items: []comparison.Row{}}
		for _, row := range stored.rows {
			if row.Number > q.After && len(page.Items) < q.Limit {
				page.Items = append(page.Items, row)
			}
		}
		if n := len(page.Items); n > 0 && page.Items[n-1].Number < stored.progress.Rows {
			page.NextAfter = page.Items[n-1].Number
		}
		return fakeReply(200, page)
	case "comparison_rows":
		if !a.modern {
			return fakeUnknownOperation()
		}
		var q comparison.RowsQuery
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		if q.After < 0 || q.Limit < 1 || q.Limit > comparison.MaxPageRows || q.MaxBytes < 0 || q.MaxBytes > comparison.MaxPageBytes {
			return invalid(fmt.Errorf("invalid rows query %+v", q))
		}
		for _, status := range q.Statuses {
			if !slices.Contains(comparison.RowStatuses, status) {
				return invalid(fmt.Errorf("unknown status %q", status))
			}
		}
		stored := a.comparisons[q.ID]
		if stored == nil {
			return notFound()
		}
		page := comparison.Page{Progress: stored.progress, Items: []comparison.Row{}}
		var examined, last int64
		for _, row := range stored.rows {
			if row.Number <= q.After {
				continue
			}
			if a.scanBudget > 0 && examined == int64(a.scanBudget) {
				break
			}
			examined, last = examined+1, row.Number
			if len(q.Statuses) == 0 || slices.Contains(q.Statuses, row.Status) {
				page.Items = append(page.Items, row)
				if len(page.Items) == q.Limit {
					break
				}
			}
		}
		if last > 0 && last < stored.progress.Rows {
			page.NextAfter = last
		}
		return fakeReply(200, page)
	case "comparison_list":
		if !a.modern {
			return fakeUnknownOperation()
		}
		var q struct{}
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		list := comparison.List{Items: []comparison.Progress{}}
		for _, stored := range a.comparisons {
			list.Items = append(list.Items, stored.progress)
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].StartedAt.After(list.Items[j].StartedAt) })
		return fakeReply(200, list)
	case "comparison_resume":
		if !a.modern {
			return fakeUnknownOperation()
		}
		var q comparison.ResumeRequest
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		stored := a.comparisons[q.ID]
		if stored == nil {
			return notFound()
		}
		if q.StorageRevision == 0 || q.StorageRevision != a.revision {
			return fakeReply(409, map[string]string{"code": "operation_failed", "message": storageconfig.ErrStaleRevision.Error()})
		}
		for id, other := range a.comparisons {
			if id != q.ID && other.progress.Status == comparison.StateRunning {
				return fakeReply(429, map[string]string{"message": "Another comparison is running on this node"})
			}
		}
		if !stored.progress.Resumable {
			return fakeReply(409, map[string]string{"message": "Comparison cannot be resumed"})
		}
		stored.progress.Status, stored.progress.Resumable, stored.progress.Error, stored.progress.ErrorDetail = comparison.StateRunning, false, "", ""
		stored.progress.Resumes++
		return fakeReply(200, stored.progress)
	case "comparison_cancel":
		var q struct {
			ID string `json:"id"`
		}
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		stored := a.comparisons[q.ID]
		if stored == nil {
			return notFound()
		}
		if stored.progress.Status == comparison.StateRunning {
			stored.progress.Status = comparison.StateCanceled
			stored.progress.Resumable = a.modern
		}
		return fakeReply(200, stored.progress)
	case "comparison_delete":
		var q struct {
			ID string `json:"id"`
		}
		if err := fakeDecode(c.Payload, &q); err != nil {
			return invalid(err)
		}
		if stored := a.comparisons[q.ID]; stored != nil && stored.progress.Status == comparison.StateRunning {
			if a.modern {
				return fakeReply(409, map[string]string{"message": "Comparison is running; cancel it before deleting"})
			}
			return fakeReply(429, map[string]string{"message": "Another comparison is running on this node"})
		}
		delete(a.comparisons, q.ID)
		return fakeReply(200, map[string]bool{"success": true})
	}
	return fakeUnknownOperation()
}

// put stores a comparison with rows numbered 1..len(rows) after it.
func (a *fakeComparisonAgent) put(p comparison.Progress, rows []comparison.Row) {
	a.mu.Lock()
	defer a.mu.Unlock()
	numbered := make([]comparison.Row, len(rows))
	for i, row := range rows {
		row.Number = int64(i + 1)
		numbered[i] = row
	}
	a.comparisons[p.ID] = &fakeComparison{progress: p, rows: numbered}
}

func (a *fakeComparisonAgent) progress(id string) (comparison.Progress, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	stored := a.comparisons[id]
	if stored == nil {
		return comparison.Progress{}, false
	}
	return stored.progress, true
}

func (a *fakeComparisonAgent) setIntercept(intercept func(agenttransport.Command) (agenttransport.Response, bool)) {
	a.mu.Lock()
	a.intercept = intercept
	a.mu.Unlock()
}

// calledWith returns the payloads of every call of operation, in order.
func (a *fakeComparisonAgent) calledWith(operation string) []json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	var payloads []json.RawMessage
	for _, call := range a.calls {
		if call.operation == operation {
			payloads = append(payloads, call.payload)
		}
	}
	return payloads
}

func (a *fakeComparisonAgent) operations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var operations []string
	for _, call := range a.calls {
		operations = append(operations, call.operation)
	}
	return operations
}

func (a *fakeComparisonAgent) resetCalls() {
	a.mu.Lock()
	a.calls = nil
	a.mu.Unlock()
}

// comparisonTestNode enrolls name and runs executor for it until the test
// ends.
func comparisonTestNode(t *testing.T, f *agentHTTPFixture, name string, executor agenttransport.Executor) task.Node {
	t.Helper()
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, name+".example"), name, credential)
	if code != 200 {
		t.Fatal(code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, f, credential), executor, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second): // hang guard only
			t.Error("comparison test Agent did not stop")
		}
	})
	node := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}
	waitOnline(t, f, node)
	return node
}

// offlineComparisonNode enrolls name without running it and forgets the
// heartbeat its enrollment counted as.
func offlineComparisonNode(t *testing.T, f *agentHTTPFixture, name string) task.Node {
	t.Helper()
	code, session := f.join(t, f.invite(t, name+".example"), name, agentCredential(t))
	if code != 200 {
		t.Fatal(code)
	}
	f.master.agents.mu.Lock()
	delete(f.master.agents.lastSeen, session.NodeID)
	f.master.agents.mu.Unlock()
	return task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}
}

func comparisonTestRequest(id string) comparison.Request {
	return comparison.Request{ID: id, StorageRevision: 1, Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, SourcePrefix: "src/", DestinationPrefix: "dst/", Mode: "content", Scope: "prefix"}
}

func putComparisonAssignment(t *testing.T, f *agentHTTPFixture, node task.Node, request comparison.Request, created time.Time) {
	t.Helper()
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put(comparisonAssignmentsBucket, request.ID, comparisonAssignment{Request: request, NodeID: node.ID, EnrollmentID: node.EnrollmentID, CreatedAt: created})
	}); err != nil {
		t.Fatal(err)
	}
}

func comparisonAssignmentExists(t *testing.T, f *agentHTTPFixture, id string) bool {
	t.Helper()
	found := false
	if err := f.db.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get(comparisonAssignmentsBucket, id, &comparisonAssignment{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// fakeComparisonRows builds n rows whose statuses cycle through
// comparison.RowStatuses.
func fakeComparisonRows(n int) []comparison.Row {
	rows := make([]comparison.Row, n)
	for i := range rows {
		key := fmt.Sprintf("object/%06d", i+1)
		rows[i] = comparison.Row{Key: key, SourceKey: "src/" + key, DestinationKey: "dst/" + key, Status: comparison.RowStatuses[i%len(comparison.RowStatuses)], SourceSize: int64(i), DestinationSize: int64(i + 1)}
		switch rows[i].Status {
		case comparison.StatusDifferent:
			rows[i].Reasons = []string{comparison.ReasonSize, comparison.ReasonContentType}
		case comparison.StatusETagMismatch:
			rows[i].Reasons = []string{comparison.ReasonETag}
		case comparison.StatusError:
			rows[i].Error = "Cannot read object metadata"
			rows[i].ErrorDetail = "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)"
		}
	}
	return rows
}

// completedComparison is the Progress of a finished comparison that stored
// every row.
func completedComparison(id string, rows []comparison.Row) comparison.Progress {
	p := comparison.Progress{Request: comparisonTestRequest(id), Status: comparison.StateCompleted, Rows: int64(len(rows)), ResultsProtocol: comparison.Protocol, StartedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	for _, row := range rows {
		p.Compared++
		switch row.Status {
		case comparison.StatusDifferent:
			p.Different++
		case comparison.StatusETagMismatch:
			p.ETagMismatch++
		case comparison.StatusMissingTarget:
			p.Missing++
		case comparison.StatusExtraTarget:
			p.Extra++
		case comparison.StatusChanged:
			p.Changed++
		case comparison.StatusBothMissing:
			p.BothMissing++
		default:
			p.Errors++
		}
	}
	p.Compared += 5
	p.Matched = 5
	return p
}
