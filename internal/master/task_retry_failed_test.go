package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// retryStorage serves real S3 wire requests (takeoverStorage) and injects the
// failure of one object's PUT. The second LIST page waits for listGate, so
// the scan stays incomplete and the parent keeps RUNNING after its first
// child ends with one failed object, as in a long migration.
type retryStorage struct {
	*takeoverStorage
	failing    atomic.Bool
	failures   atomic.Int64
	hold       atomic.Bool
	listGate   chan struct{}
	held       chan struct{}
	cancelled  chan struct{}
	heldOnce   sync.Once
	cancelOnce sync.Once
}

const retryFailedKey = "objects/001"

func newRetryStorage(objects int) *retryStorage {
	base := &takeoverStorage{source: map[string][]byte{}, target: map[string][]byte{}, reads: map[string]int{}, writes: map[string]int{},
		readStarted: make(chan struct{}), readCancelled: make(chan struct{}), listStarted: make(chan struct{}), listCancelled: make(chan struct{})}
	for i := range objects {
		base.source[fmt.Sprintf("objects/%03d", i)] = bytes.Repeat([]byte(fmt.Sprintf("payload-%03d", i)), 16)
	}
	s := &retryStorage{takeoverStorage: base, listGate: make(chan struct{}), held: make(chan struct{}), cancelled: make(chan struct{})}
	s.failing.Store(true)
	return s
}

func (s *retryStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "source" && r.Method == http.MethodGet && key == "" && r.URL.Query().Get("continuation-token") != "" {
		select {
		case <-s.listGate:
		case <-r.Context().Done():
			return
		}
	}
	if bucket == "target" && r.Method == http.MethodPut && key == retryFailedKey {
		if s.failing.Load() {
			_, _ = io.Copy(io.Discard, r.Body)
			s.failures.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>injected failure</Message></Error>`)
			return
		}
		if s.hold.Load() {
			// The server notices a closed client connection only after the
			// request body was read; then the context ends when the Agent
			// cancels the upload.
			_, _ = io.Copy(io.Discard, r.Body)
			s.heldOnce.Do(func() { close(s.held) })
			<-r.Context().Done()
			s.cancelOnce.Do(func() { close(s.cancelled) })
			return
		}
	}
	s.takeoverStorage.ServeHTTP(w, r)
}

func (s *retryStorage) counts(key string) (reads, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[key], s.writes[key]
}

// retryAgentExecutor advertises the Agent's capabilities; an old Agent omits
// retryFailedProtocol, as releases before child_retry_failed do.
type retryAgentExecutor struct {
	*agentservice.Service
	old bool
}

func (a retryAgentExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		caps := a.LeaseCapabilities()
		if a.old {
			delete(caps, "retryFailedProtocol")
		}
		raw, _ := json.Marshal(caps)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	}
	if a.old && c.Operation == "child_retry_failed" {
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
	return a.Service.Execute(ctx, c)
}

type retryFixture struct {
	f       *agentHTTPFixture
	storage *retryStorage
	agent   *agentservice.Service
	parent  task.Parent
	runtime *taskRuntime
	ctx     context.Context
}

// newRetryFixture runs a single-node OSS-to-OSS task over the authenticated
// Agent channel until its first child (128 objects) ends PARTIAL_FAILED with
// one failed object while the parent keeps RUNNING.
func newRetryFixture(t *testing.T, old bool) *retryFixture {
	t.Helper()
	f := newAgentHTTPFixture(t)
	storage := newRetryStorage(130)
	s3 := httptest.NewServer(storage)
	t.Cleanup(func() {
		select {
		case <-storage.listGate:
		default:
			close(storage.listGate)
		}
		s3.Close()
	})
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, "retry-a.example"), "retry-a", credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, f, credential), retryAgentExecutor{service, old}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { done <- runner.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done; service.Close(context.Background()); db.Close() })
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{
		"alias": "qa", "endpoint": s3.URL, "accessKey": "fixture-ak", "secretKey": "fixture-sk", "region": "test", "pathStyleEnable": true, "validate": false,
	})
	if code != http.StatusOK {
		t.Fatal("storage setup", code, body)
	}
	node := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "retry-a.example"}
	p := takeoverParent(t, f, node)
	// A hang guard for slow, race-instrumented runners, not a performance bound.
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(stop)
	r := &retryFixture{f: f, storage: storage, agent: service, parent: p, runtime: f.master.tasks, ctx: ctx}
	p, child := r.drive(t, func(p task.Parent, child task.Child) bool {
		return child.Record != nil && child.Record.Status == execution.PartialFailed
	})
	if p.Status != "RUNNING" || p.Counters.Succeeded != 127 || p.Counters.Failed != 1 || p.FailedChildren != 1 || p.FinishedChildren != 1 || child.Record.Counters.Failed != 1 {
		t.Fatal("fixture did not leave one failed object in a running task", p, child.Record)
	}
	r.parent = p
	return r
}

// drive runs reconciliation steps until done accepts the parent and child 1.
func (r *retryFixture) drive(t *testing.T, done func(task.Parent, task.Child) bool) (task.Parent, task.Child) {
	t.Helper()
	var lastErr error
	for {
		lastErr = r.runtime.step(r.ctx, r.parent.ID)
		p, err := r.runtime.repo.Get(r.parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		var child task.Child
		if children, err := r.runtime.repo.Children(p.ID, 0, 1); err == nil && len(children) == 1 {
			child = children[0]
		}
		if done(p, child) {
			return p, child
		}
		if r.ctx.Err() != nil {
			t.Fatal("task did not reach the expected state", p, child.Record, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *retryFixture) exception(t *testing.T) task.Exception {
	t.Helper()
	var found []task.Exception
	if err := r.runtime.repo.VisitExceptions(context.Background(), task.ExceptionFilter{ParentID: r.parent.ID}, func(e task.Exception) error {
		found = append(found, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Key != retryFailedKey {
		t.Fatal("expected one exception record for the failed object", found)
	}
	return found[0]
}

func (r *retryFixture) retry(t *testing.T) map[string]any {
	t.Helper()
	url := fmt.Sprintf("%s/api/master/tasks/%d/retry-failed", r.f.http.URL, r.parent.ID)
	code, body := request(t, r.f.http.Client(), "POST", url, r.f.admin, map[string]any{"expectedGeneration": r.parent.Generation})
	if code != http.StatusOK {
		t.Fatal("retry request failed", code, body)
	}
	return body["data"].(map[string]any)
}

func TestRunningTaskRetriesOnlyFailedObjectsInPlace(t *testing.T) {
	r := newRetryFixture(t, false)
	ended, err := r.agent.Jobs.Get(context.Background(), fmt.Sprintf("%d-1", r.parent.ID))
	if err != nil || ended.Status != execution.PartialFailed {
		t.Fatal(ended, err)
	}
	if r.exception(t).RetryStatus != task.ExceptionNotRetried {
		t.Fatal("failure was recorded as retried before any retry")
	}
	r.storage.failing.Store(false)
	result := r.retry(t)
	if result["accepted"] != float64(1) || result["retriedObjects"] != float64(1) || result["failed"] != float64(0) || result["status"] != "RUNNING" || result["generation"] != float64(r.parent.Generation) {
		t.Fatal("running retry was not accepted", result)
	}
	children := result["children"].([]any)
	if len(children) != 1 || children[0].(map[string]any)["outcome"] != "QUEUED" || children[0].(map[string]any)["failedObjectCount"] != float64(1) {
		t.Fatal("child outcome missing", result)
	}
	p, err := r.runtime.repo.Get(r.parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The failed object is pending again; succeeded credit is unchanged and the
	// parent cannot finish while the child runs.
	if p.Status != "RUNNING" || p.Generation != r.parent.Generation || p.Counters.Failed != 0 || p.Counters.FailedBytes != 0 || p.FailedChildren != 0 || p.Counters.Succeeded < 127 {
		t.Fatal("retry left an inexact ledger", p.Status, p.Counters, p.FailedChildren, p.FinishedChildren)
	}
	if entry := r.exception(t); entry.RetryCount != 1 || (entry.RetryStatus != task.ExceptionPending && entry.RetryStatus != task.ExceptionRecovered) {
		t.Fatal("exception record does not show the retry", entry)
	}
	// A repeated request reopens nothing.
	again := r.retry(t)
	if again["accepted"] != float64(0) || again["failed"] != float64(0) || again["message"] == "" {
		t.Fatal("repeated retry was not ignored", again)
	}
	reopened, err := r.agent.Jobs.Get(context.Background(), ended.Job.ChildID)
	if err != nil || reopened.Attempt > ended.Attempt+2 || reopened.Job.Generation != ended.Job.Generation {
		t.Fatal("repeated retry reopened the child again", ended.Attempt, reopened.Attempt, err)
	}
	p, child := r.drive(t, func(p task.Parent, child task.Child) bool {
		return child.Record != nil && child.Record.Status == execution.Completed
	})
	if p.Status != "RUNNING" || p.Counters.Succeeded != 128 || p.Counters.Failed != 0 || p.FailedChildren != 0 || p.FinishedChildren != 1 || child.Record.Counters.Succeeded != 128 {
		t.Fatal("retried child did not complete exactly", p.Counters, p.FailedChildren, child.Record.Counters)
	}
	if _, writes := r.storage.counts(retryFailedKey); writes != 1 || r.storage.failures.Load() != 1 {
		t.Fatal("failed object was not written exactly once after the retry", writes, r.storage.failures.Load())
	}
	if reads, writes := r.storage.counts("objects/000"); reads != 1 || writes != 1 {
		t.Fatal("retry transferred a succeeded object again", reads, writes)
	}
	if entry := r.exception(t); entry.RetryStatus != task.ExceptionRecovered || entry.RetryCount != 1 {
		t.Fatal("exception record does not show the recovery", entry)
	}
	close(r.storage.listGate)
	p, _ = r.drive(t, func(p task.Parent, _ task.Child) bool { return task.Ended(p.Status) })
	if p.Status != "COMPLETED" || p.Counters.Succeeded != 130 || p.Counters.Failed != 0 || p.FailedChildren != 0 || p.ChildCount != 2 {
		t.Fatal("task did not finish with an exact ledger", p.Status, p.Counters, p.ChildCount)
	}
}

func TestRunningTaskRetryIsFencedByStop(t *testing.T) {
	r := newRetryFixture(t, false)
	r.storage.failing.Store(false)
	r.storage.hold.Store(true)
	// The bulk retry route retries a running task's failed objects in place.
	code, body := request(t, r.f.http.Client(), "POST", r.f.http.URL+"/api/master/retry", r.f.admin, map[string]any{
		"taskIds": []int64{r.parent.ID}, "expectedGenerations": map[string]uint64{fmt.Sprint(r.parent.ID): r.parent.Generation},
	})
	details, _ := body["details"].([]any)
	if code != http.StatusOK || body["accepted"] != float64(1) || len(details) != 1 {
		t.Fatal("bulk retry of a running task was not accepted", code, body)
	}
	detail := details[0].(map[string]any)
	if detail["mode"] != "FAILED_OBJECTS" || detail["retriedChildren"] != float64(1) || detail["retriedObjects"] != float64(1) || detail["generation"] != float64(r.parent.Generation) {
		t.Fatal("bulk retry did not report the in-place retry", detail)
	}
	select {
	case <-r.storage.held:
	case <-r.ctx.Done():
		t.Fatal("reopened child did not start its retry")
	}
	code, body = request(t, r.f.http.Client(), "POST", r.f.http.URL+"/api/master/stop", r.f.admin, map[string]any{"taskIds": []int64{r.parent.ID}})
	if code != http.StatusOK || body["accepted"] != float64(1) {
		t.Fatal("stop was not accepted", code, body)
	}
	p, child := r.drive(t, func(p task.Parent, _ task.Child) bool { return p.Status == "STOPPED" })
	select {
	case <-r.storage.cancelled:
	case <-r.ctx.Done():
		t.Fatal("stop did not cancel the retried transfer")
	}
	record, err := r.agent.Jobs.Get(context.Background(), child.Job.ChildID)
	if err != nil || record.Status != execution.Stopped || child.Record.Status != execution.Stopped {
		t.Fatal("retried child was not fenced by the stop", record.Status, child.Record.Status, err)
	}
	if p.Counters.Succeeded != 127 || p.Counters.Failed != 0 || p.FailedChildren != 0 {
		t.Fatal("stopped ledger is inexact", p.Counters, p.FailedChildren)
	}
	if _, writes := r.storage.counts(retryFailedKey); writes != 0 {
		t.Fatal("fenced retry wrote the object", writes)
	}
	if entry := r.exception(t); entry.RetryStatus != task.ExceptionInterrupted || entry.RetryCount != 1 {
		t.Fatal("interrupted retry is not visible", entry)
	}
	url := fmt.Sprintf("%s/api/master/tasks/%d/retry-failed", r.f.http.URL, r.parent.ID)
	if code, body := request(t, r.f.http.Client(), "POST", url, r.f.admin, nil); code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["message"]), "Only a running task") {
		t.Fatal("stopped task accepted an in-place retry", code, body)
	}
}

func TestRunningTaskRetryRefusesOldAgent(t *testing.T) {
	r := newRetryFixture(t, true)
	before, err := r.agent.Jobs.Get(context.Background(), fmt.Sprintf("%d-1", r.parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	r.storage.failing.Store(false)
	result := r.retry(t)
	if result["accepted"] != float64(0) || result["failed"] != float64(1) || !strings.Contains(fmt.Sprint(result["message"]), "Upgrade Agent") {
		t.Fatal("old Agent was not refused with an actionable reason", result)
	}
	after, err := r.agent.Jobs.Get(context.Background(), before.Job.ChildID)
	if err != nil || after.Status != execution.PartialFailed || after.Attempt != before.Attempt {
		t.Fatal("refused retry changed the Agent ledger", before, after, err)
	}
	p, err := r.runtime.repo.Get(r.parent.ID)
	if err != nil || p.Counters.Failed != 1 || p.FailedChildren != 1 || p.FinishedChildren != 1 {
		t.Fatal("refused retry changed the Master ledger", p.Counters, err)
	}
	if entry := r.exception(t); entry.RetryStatus != task.ExceptionNotRetried || entry.RetryCount != 0 {
		t.Fatal("refused retry was recorded as a retry", entry)
	}
}
