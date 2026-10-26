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
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed/clear", f.admin, map[string]any{"taskIndex": ids[0]})
	if code != 200 || body["clearedCount"] != float64(2) || body["executionLedgerPreserved"] != true {
		t.Fatal(code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{"limit": 1000})
	if code != 200 || body["totalCount"] != float64(2) {
		t.Fatal("clear crossed task filter", code, body)
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
