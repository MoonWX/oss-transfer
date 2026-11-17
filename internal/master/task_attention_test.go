package master

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// The task detail names the main cause of its failed objects and says when
// every node it runs on is offline, so the operator need not open each
// failed object to learn why.
func TestTaskDetailSummarisesFailuresAndOfflineNodes(t *testing.T) {
	f := newAgentHTTPFixture(t)
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: "node-stopped", EnrollmentID: "enrolled", Address: "stopped.example"}}
	p, _, err := f.master.tasks.repo.Create(spec, "attention")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.master.tasks.repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "a", Size: 3, ETag: "a"}, {Key: "b", Size: 5, ETag: "b"}, {Key: "c", Size: 7, ETag: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 2, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 3, TotalBytes: 15, Failed: 3, FailedBytes: 15}}
	errors := []string{`permission denied: write destination "a": mkdirat medium: permission denied`, `permission denied: write destination "b": mkdirat medium: permission denied`, `open source "c": source changed`}
	for i, item := range c.Job.Items {
		result := execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: errors[i]}
		if i == 2 {
			result.Reconciliation = execution.ReasonSourceChanged
		}
		record.Results = append(record.Results, result)
	}
	if err := f.master.tasks.repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	code, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	row := body["data"].(map[string]any)
	summary, ok := row["failureSummary"].(map[string]any)
	if !ok || summary["failed"] != float64(3) || summary["sampled"] != float64(3) {
		t.Fatalf("failure summary: %+v", row["failureSummary"])
	}
	causes := summary["causes"].([]any)
	first, second := causes[0].(map[string]any), causes[1].(map[string]any)
	if len(causes) != 2 || first["code"] != "LOCAL_PERMISSION_DENIED" || first["count"] != float64(2) || second["code"] != "SOURCE_CHANGED" || second["count"] != float64(1) {
		t.Fatalf("failure causes: %+v", causes)
	}
	if message, _ := summary["message"].(string); !strings.Contains(message, "3") || !strings.Contains(message, first["message"].(string)) {
		t.Fatalf("summary message: %q", message)
	}
	t.Logf("task status %v", row["status"])
	offline, _ := row["offlineNodes"].([]any)
	attention, _ := row["attention"].(map[string]any)
	if task.Ended(stringValue(row["status"])) {
		if offline != nil || attention["code"] != "OBJECTS_FAILED" {
			t.Fatalf("ended task: %+v %+v", offline, attention)
		}
		return
	}
	if len(offline) != 1 || offline[0].(map[string]any)["executionNodeIp"] != "stopped.example" || attention["code"] != "NODES_OFFLINE" || !strings.Contains(attention["message"].(string), "stopped.example") {
		t.Fatalf("offline node: %+v %+v", offline, attention)
	}
}

func TestFailureCausesPreferTheRetryOutcome(t *testing.T) {
	for _, test := range []struct {
		exception task.Exception
		code      string
	}{
		{task.Exception{Error: "dial tcp 10.0.0.1:9000: connect: connection refused"}, "CONNECTION_REFUSED"},
		{task.Exception{Error: "lookup bucket.minio: no such host"}, "HOST_NOT_FOUND"},
		{task.Exception{Error: "no space left on device: write"}, "NO_SPACE"},
		{task.Exception{Error: "api error AccessDenied: denied"}, "ACCESS_DENIED"},
		{task.Exception{Error: "api error InvalidAccessKeyId"}, "INVALID_CREDENTIALS"},
		{task.Exception{Error: "source changed", Reason: execution.ReasonSourceChanged, RetryStatus: task.ExceptionRetryFailed, RetryError: "connection refused"}, "CONNECTION_REFUSED"},
		{task.Exception{Error: "something unexpected"}, "OTHER"},
	} {
		if code, _, _ := failureCause(test.exception); code != test.code {
			t.Fatalf("%+v: %s, want %s", test.exception, code, test.code)
		}
	}
}

// Object failures as the Agent records them: the stage and quoted key, then
// the SDK's text. The cause comes from the storage reply's error code, its
// status only for a reply without one, and never from the object key: the
// key of most cases below contains "eof".
func TestFailureCausesClassifyStorageRepliesByCodeNotByKey(t *testing.T) {
	const reply = "https response error StatusCode: %d, RequestID: REQ1, HostID: H1, %s [storage response: http=%d, content-type=application/xml, protocol=HTTP/1.1, connection=reused, idle-ms=0, request-length=0, body=xml]"
	get := func(status int, code string) string {
		return `open source "videofiles/Geoffrey.mp4": operation error S3: GetObject, ` + fmt.Sprintf(reply, status, code, status)
	}
	put := func(status int, code string) string {
		return `write destination "videofiles/Geoffrey.mp4": operation error S3: PutObject, exceeded maximum number of attempts, 10, ` + fmt.Sprintf(reply, status, code, status)
	}
	for _, test := range []struct{ text, code string }{
		// An archived object that was not restored: AWS Glacier and Alibaba
		// Cloud Archive objects, which the SDK models without "api error".
		{get(403, "InvalidObjectState: The operation is not valid for the object's storage class"), "ARCHIVED"},
		{get(403, "InvalidObjectState: The operation is not valid for the object's state"), "ARCHIVED"},
		{get(403, "api error AccessDenied: Access Denied"), "ACCESS_DENIED"},
		// A HEAD has no body: the status is all there is.
		{`check destination "k": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden`, "ACCESS_DENIED"},
		// Other refusals are not guessed to be about permissions.
		{get(403, "api error RequestTimeTooSkewed: The difference between the request time and the current time is too large."), "OTHER"},
		{put(403, "api error InvalidObjectState: The operation is not valid for the object's state"), "OTHER"},
		{put(503, "api error SlowDown: Please reduce your request rate."), "THROTTLED"},
		{put(429, "api error TooManyRequests: Too many requests"), "THROTTLED"},
		{put(503, "api error ServiceUnavailable: Service Unavailable"), "SERVER_ERROR"},
		{put(500, "api error InternalError: We encountered an internal error."), "SERVER_ERROR"},
		{put(502, "api error BadGateway: Bad Gateway"), "SERVER_ERROR"},
		{`write destination "k": operation error S3: PutObject, exceeded maximum number of attempts, 10, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put "http://storage.example:9000/bucket/k?x-id=PutObject": EOF`, "CONNECTION_RESET"},
		{`verify write "users/Geoffrey/EOF.txt": destination did not confirm the complete object`, "OTHER"},
		{`write destination "logs/timeout/connection reset.txt": operation error S3: PutObject, https response error StatusCode: 400, RequestID: R, HostID: H, api error InvalidArgument: bad header`, "OTHER"},
		{`open source "timeout.log": operation error S3: GetObject, https response error StatusCode: 403, RequestID: R, HostID: H, api error AccessDenied: Access Denied`, "ACCESS_DENIED"},
		{`permission denied: write destination "a": mkdirat medium: permission denied`, "LOCAL_PERMISSION_DENIED"},
	} {
		if code, message, _ := failureCause(task.Exception{Error: test.text}); code != test.code || (code != "OTHER") != (message != "") {
			t.Errorf("%s: %s %q, want %s", test.text, code, message, test.code)
		}
	}
	for code, message := range failureMessages {
		for _, locale := range []string{"en-US", "ja-JP"} {
			if localizeMessage(locale, message) == message {
				t.Errorf("%s: %q has no %s translation", code, message, locale)
			}
		}
	}
}

// The summary reads a bounded number of records, also when most of them are
// skipped: after a retry of many failures they are pending, and the detail
// view is polled. Past the bound the summary is a sample.
func TestFailureSummaryBoundsTheRecordsItVisits(t *testing.T) {
	tally := failureTally{groups: map[string]*failureGroup{}}
	for i := 0; i < failureSummaryVisits; i++ {
		if err := tally.add(task.Exception{Error: "connection refused", RetryStatus: task.ExceptionPending}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if err := tally.add(task.Exception{Error: "connection refused"}); !errors.Is(err, errSummaryFull) || len(tally.groups) != 0 {
		t.Fatalf("the visit bound was not applied: %v %+v", err, tally.groups)
	}
	tally = failureTally{groups: map[string]*failureGroup{}}
	for i := 0; i < failureSummaryScan; i++ {
		if err := tally.add(task.Exception{Error: "connection refused"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tally.add(task.Exception{Error: "connection refused"}); !errors.Is(err, errSummaryFull) || tally.groups["CONNECTION_REFUSED"].count != failureSummaryScan {
		t.Fatalf("the sample bound was not applied: %v %+v", err, tally.groups)
	}
}

// "All N objects failed" holds only when every discovered object was tried.
// A stopped task, or one whose scan did not finish, has objects that were
// never attempted, so its summary names only the failures.
func TestFailureSummarySaysAllOnlyWhenEveryObjectWasTried(t *testing.T) {
	f := newAgentHTTPFixture(t)
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: "node-a", EnrollmentID: "enrolled", Address: "a.example"}}
	p, _, err := f.master.tasks.repo.Create(spec, "all-failed")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.master.tasks.repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "a", Size: 3, ETag: "a"}, {Key: "b", Size: 5, ETag: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 2, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 2, TotalBytes: 8, Failed: 2, FailedBytes: 8}}
	for _, item := range c.Job.Items {
		record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "AccessDenied: StatusCode: 403"})
	}
	if err := f.master.tasks.repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	if p, err = f.master.tasks.repo.Get(p.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		status       string
		scanComplete bool
		all          bool
	}{
		{"FAILED", true, true},
		{"STOPPED", true, false},
		{"FAILED", false, false},
		{"PARTIAL_FAILED", false, false},
	} {
		parent := p
		parent.Status, parent.ScanComplete = test.status, test.scanComplete
		summary := f.master.failureSummary(context.Background(), parent)
		if summary == nil {
			t.Fatal("no summary", test)
		}
		message := summary["message"].(string)
		if summary["allFailed"] != test.all || strings.Contains(message, "全部") != test.all {
			t.Errorf("%s, scan complete %v: allFailed %v, %q", test.status, test.scanComplete, summary["allFailed"], message)
		}
	}
}
