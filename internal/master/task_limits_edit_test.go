package master

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestTaskRequestLimitsEditReachesAgentAndPreservesBandwidthClients(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	code, local := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/local-nodes", f.admin, nil)
	if code != 200 {
		t.Fatal(code, local)
	}
	nodes := local["data"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatal(nodes)
	}
	info := nodes[0].(map[string]any)
	if info["executionNodeIp"] != node.Address || info["hostname"] == nil || info["hostname"] == "" || info["label"] == nil || info["localIps"] == nil {
		t.Fatal("node selector lacks recognizable host metadata", info)
	}
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{node}
	parent, _, err := f.master.tasks.repo.Create(spec, "limits-edit")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, parent.ID)
	put := func(body map[string]any) map[string]any {
		t.Helper()
		status, response := request(t, f.http.Client(), "PUT", url, f.admin, body)
		if status != 200 || response["data"].(map[string]any)["applied"] != true {
			t.Fatal(status, response)
		}
		return response["data"].(map[string]any)
	}
	put(map[string]any{"limitMbps": 100, "requestLimits": map[string]any{"scope": "CLUSTER", "listQps": 10, "headQps": 20, "putQps": 30}})
	policy, err := service.Limits.Parent(parent.ID)
	if err != nil || policy.ListQPS != 10 || policy.HeadQPS != 20 || policy.PutQPS != 30 || policy.BandwidthBytesPerSecond != 12500000 {
		t.Fatal(policy, err)
	}
	put(map[string]any{"limitMbps": 50})
	policy, _ = service.Limits.Parent(parent.ID)
	if policy.ListQPS != 10 || policy.PutQPS != 30 || policy.BandwidthBytesPerSecond != 6250000 {
		t.Fatal("old bandwidth client erased QPS", policy)
	}
	put(map[string]any{"requestLimitsPatch": map[string]any{"headQps": 99}})
	policy, _ = service.Limits.Parent(parent.ID)
	if policy.ListQPS != 10 || policy.HeadQPS != 99 || policy.PutQPS != 30 || policy.BandwidthBytesPerSecond != 6250000 {
		t.Fatal("patch changed unrelated caps", policy)
	}
	put(map[string]any{"requestLimits": nil})
	policy, _ = service.Limits.Parent(parent.ID)
	if policy.ListQPS != 0 || policy.PutQPS != 0 || policy.BandwidthBytesPerSecond != 6250000 {
		t.Fatal("clear QPS lost bandwidth", policy)
	}
	before := policy
	for _, body := range []map[string]any{{"requestLimits": map[string]any{"putQps": -1}}, {"requestLimits": map[string]any{"typoQps": 10}}, {}} {
		code, _ := request(t, f.http.Client(), "PUT", url, f.admin, body)
		if code != 400 {
			t.Fatal("invalid limits accepted", code)
		}
	}
	policy, _ = service.Limits.Parent(parent.ID)
	if policy != before {
		t.Fatal("rejected request changed Agent limits")
	}
	// Matching revision and bandwidth alone must not confirm mismatched QPS.
	policy.PutQPS = 99
	policy.Revision++
	if err := service.Limits.ApplyParent(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	_, response := request(t, f.http.Client(), "GET", url, f.admin, nil)
	if response["data"].(map[string]any)["applied"] != false {
		t.Fatal("divergent Agent policy reported applied")
	}
	if _, err := f.master.tasks.repo.Stop(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(parent.ID, parent.Generation); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(t, f.http.Client(), "PUT", url, f.admin, map[string]any{"requestLimits": nil}); code != 409 {
		t.Fatal("ended task allowed edits", code)
	}
}

func TestActiveChildrenCursorSkipsCompletedAndKeepsStableNumbers(t *testing.T) {
	f := newAgentHTTPFixture(t)
	p := persistAPITask(t, f, "hot-children")
	for number := 1; number <= 80; number++ {
		child, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, []transferstore.Object{{Key: fmt.Sprint(number), Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if number <= 70 {
			record := execution.Record{Job: child.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1}}
			if err := f.master.tasks.repo.Observe(p.ID, p.Generation, number, record); err != nil {
				t.Fatal(err)
			}
		}
	}
	get := func(query string) map[string]any {
		t.Helper()
		code, response := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?%s", f.http.URL, p.ID, query), f.admin, nil)
		if code != 200 {
			t.Fatal(code, response)
		}
		return response["data"].(map[string]any)
	}
	first := get("view=active&offset=0&limit=3")
	if first["totalCount"] != float64(10) || first["nextOffset"] != float64(73) || first["hasMore"] != true {
		t.Fatal(first)
	}
	items := first["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["subtaskNumber"] != float64(71) {
		t.Fatal(items)
	}
	// Completion before the cursor must not shift subsequent pages backward.
	children, _ := f.master.tasks.repo.Children(p.ID, 70, 1)
	c := children[0]
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 71, execution.Record{Job: c.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1}}); err != nil {
		t.Fatal(err)
	}
	last := get("view=active&offset=73&limit=25")
	if last["hasMore"] != false || last["nextOffset"] != float64(80) || len(last["items"].([]any)) != 7 {
		t.Fatal(last)
	}
	all := get("offset=0&limit=25")
	if len(all["items"].([]any)) != 25 || all["nextOffset"] != float64(25) || all["totalCount"] != float64(80) {
		t.Fatal(all)
	}
}
