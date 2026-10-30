package master

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestExceptionsHTTPFiltersClearAndLocalEnvelope(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ids := []int64{}
	for index, direction := range []string{"OSS_TO_OSS", "LOCAL_TO_OSS"} {
		spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
		if err != nil {
			t.Fatal(err)
		}
		spec.Direction = direction
		if index == 1 {
			spec.Source = execution.Endpoint{Kind: "LOCAL", Root: "/data/upload"}
		}
		spec.Nodes = []task.Node{{ID: fmt.Sprintf("node-%d", index), EnrollmentID: "enrolled", Address: fmt.Sprintf("node-%d.example", index)}}
		p, _, err := f.master.tasks.repo.Create(spec, fmt.Sprintf("failure-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		c, err := f.master.tasks.repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "first", Size: 3}, {Key: "second", Size: 5}})
		if err != nil {
			t.Fatal(err)
		}
		record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 2, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 2, TotalBytes: 8, Failed: 2, FailedBytes: 8}}
		for _, item := range c.Job.Items {
			record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "denied: " + item.Source.Key})
		}
		if err := f.master.tasks.repo.Observe(p.ID, 1, 1, record); err != nil {
			t.Fatal(err)
		}
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{"taskIndex": ids[0], "workerNode": "node-0.example", "limit": 1})
	groups := body["failedKeys"].(map[string]any)
	if code != 200 || body["totalCount"] != float64(2) || body["truncated"] != true || len(groups["node-0.example"].([]any)) != 1 || len(groups) != 2 {
		t.Fatal("task/node filtering or per-node limit incorrect", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/local-transfers/failures?taskId=%d&direction=LOCAL_TO_OSS&executionNodeIp=node-1.example&offset=1&limit=1", f.http.URL, ids[1]), f.admin, nil)
	data := body["data"].(map[string]any)
	items := data["items"].([]any)
	if code != 200 || data["total"] != float64(2) || len(items) != 1 || items[0].(map[string]any)["localPath"] == "" {
		t.Fatal("local exception paging incorrect", code, body)
	}
	if row := items[0].(map[string]any); row["retryStatus"] != task.ExceptionNotRetried || row["retryCount"] != float64(0) {
		t.Fatal("Agent attempt was exposed as object retry count", row)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed/clear", f.admin, map[string]any{"taskIndex": ids[0]})
	if code != 200 || body["clearedCount"] != float64(0) || body["executionLedgerPreserved"] != true || body["scope"] != "resolved-exceptions" || body["complete"] != true {
		t.Fatal(code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{"limit": 1000})
	if code != 200 || body["totalCount"] != float64(4) {
		t.Fatal("clear hid unresolved failures", code, body)
	}
	parent, err := f.master.tasks.repo.Get(ids[0])
	if err != nil || parent.Counters.Failed != 2 {
		t.Fatal("clear destroyed task evidence", parent, err)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/fallback", f.admin, map[string]any{})
	if code != 200 || body["supported"] != false || body["totalCount"] != float64(0) || body["complete"] != true {
		t.Fatal("legacy fallback invented an event", code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/retry-failures", f.admin, map[string]any{"taskIds": []int64{ids[1]}})
	result := body["data"].(map[string]any)
	if code != 200 || result["failed"] != float64(0) || result["ignored"] != float64(1) || result["accepted"] != float64(0) {
		t.Fatal("running task retry was not ignored in the local envelope", code, body)
	}
	if code, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed/clear", "", nil); code != 401 {
		t.Fatal("unauthenticated clear", code)
	}
}

func TestExceptionDTOSeparatesOriginalErrorAndRetryOutcome(t *testing.T) {
	original := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	retried := original.Add(time.Minute)
	entry := task.Exception{ParentID: 1, ChildID: "1-1", Attempt: 15, Error: "original HTTP 400", ObservedAt: original, RetryCount: 1, RetryGeneration: 2, RetryStatus: task.ExceptionRetryFailed, RetryError: "permission denied", RetryUpdatedAt: retried, RetryNode: "node-b"}
	row := exceptionDTO(entry)
	if row["errorMessage"] != "original HTTP 400" || row["timestamp"] != original.UnixMilli() || row["retryError"] != "permission denied" || row["retryCount"] != uint64(1) || row["retryStatus"] != task.ExceptionRetryFailed || row["retryUpdatedAt"] != retried.UnixMilli() || row["retryGeneration"] != uint64(2) || row["retryExecutionNodeIp"] != "node-b" {
		t.Fatal("retry DTO changed original evidence or omitted outcome", row)
	}
	legacy := exceptionDTO(task.Exception{Attempt: 15, ObservedAt: original})
	if legacy["retryStatus"] != task.ExceptionNotRetried || legacy["retryCount"] != uint64(0) || legacy["retryUpdatedAt"] != nil {
		t.Fatal("legacy record invented retry evidence", legacy)
	}
}

func TestResolvedExceptionCleanupIsFilteredAndCountsBeyondPage(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ids := []int64{}
	for index, direction := range []string{"OSS_TO_OSS", "LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
		spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
		if err != nil {
			t.Fatal(err)
		}
		spec.Direction = direction
		if direction == "LOCAL_TO_OSS" {
			spec.Source = execution.Endpoint{Kind: "LOCAL", Root: "/data/upload"}
		} else if direction == "OSS_TO_LOCAL" {
			spec.Destination = execution.Endpoint{Kind: "LOCAL", Root: "/data/download"}
		}
		spec.Nodes = []task.Node{{ID: fmt.Sprintf("node-%d", index), EnrollmentID: "enrolled", Address: fmt.Sprintf("node-%d.example", index)}}
		if direction == "LOCAL_TO_OSS" {
			spec.Nodes[0].LocalRoot = spec.Source.Root
		} else if direction == "OSS_TO_LOCAL" {
			spec.Nodes[0].LocalRoot = spec.Destination.Root
		}
		p, _, err := f.master.tasks.repo.Create(spec, fmt.Sprintf("resolved-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		c, err := f.master.tasks.repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "first", Size: 3}, {Key: "second", Size: 5}, {Key: "third", Size: 7}})
		if err != nil {
			t.Fatal(err)
		}
		record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC()}
		for _, item := range c.Job.Items {
			record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "denied"})
		}
		if err := f.master.tasks.repo.Observe(p.ID, 1, 1, record); err != nil {
			t.Fatal(err)
		}
		record.Attempt++
		record.UpdatedAt = record.UpdatedAt.Add(time.Second)
		record.Results[0].Status, record.Results[0].Error = execution.ItemSucceeded, ""
		record.Results[1].Status, record.Results[1].Error = execution.ItemSkipped, ""
		if err := f.master.tasks.repo.Observe(p.ID, 1, 1, record); err != nil {
			t.Fatal(err)
		}
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{"taskIndex": ids[1], "limit": 1})
	if code != 200 || body["totalCount"] != float64(3) || body["clearableCount"] != float64(2) || body["truncated"] != true {
		t.Fatal("cleanup count was limited to loaded records", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/local-transfers/failures?taskId=%d&offset=1&limit=1", f.http.URL, ids[1]), f.admin, nil)
	data := body["data"].(map[string]any)
	if code != 200 || data["total"] != float64(3) || data["clearableCount"] != float64(2) || len(data["items"].([]any)) != 1 {
		t.Fatal("local cleanup count was limited to current page", code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed/clear", f.admin, map[string]any{"type": "local", "taskIndex": ids[0]})
	if code != 200 || body["clearedCount"] != float64(0) {
		t.Fatal("local clear reached an OSS task", code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed/clear", f.admin, map[string]any{"type": "local", "taskIndex": ids[1], "executionNodeIp": "node-1.example", "direction": "LOCAL_TO_OSS", "endpoint": "/data/upload"})
	if code != 200 || body["clearedCount"] != float64(2) || body["complete"] != true || body["scope"] != "resolved-exceptions" {
		t.Fatal("resolved cleanup with offline Agent failed", code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{})
	if code != 200 || body["totalCount"] != float64(7) || body["clearableCount"] != float64(4) {
		t.Fatal("cleanup crossed task/type/node filters or removed unresolved work", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/exceptions/tasks?type=local&limit=1", f.admin, nil)
	data = body["data"].(map[string]any)
	options := data["items"].([]any)
	if code != 200 || len(options) != 1 || data["hasMore"] != true || options[0].(map[string]any)["taskIndex"] != float64(ids[2]) {
		t.Fatal("local task option limit/order incorrect", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/exceptions/tasks?type=failed&search=%%23%d", f.http.URL, ids[0]), f.admin, nil)
	data = body["data"].(map[string]any)
	if code != 200 || len(data["items"].([]any)) != 1 || data["hasMore"] != false {
		t.Fatal("task ID search", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/exceptions/tasks?type=fallback", f.admin, nil)
	if code != 200 || len(body["data"].(map[string]any)["items"].([]any)) != 0 {
		t.Fatal("unsupported fallback exposed unrelated task options", code, body)
	}
	for _, query := range []string{"type=other", "limit=101", "search=not-a-task", "search=-1"} {
		if code, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/exceptions/tasks?"+query, f.admin, nil); code != 400 {
			t.Fatal("invalid options query accepted", query, code, body)
		}
	}
	if code, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/exceptions/tasks", "", nil); code != 401 {
		t.Fatal("unauthenticated task options", code)
	}
}
