package master

import (
	"fmt"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestTaskHTTPNewestPagesRecycleAndDirectDetail(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var parents []task.Parent
	for i := range 4 {
		parents = append(parents, persistAPITask(t, f, fmt.Sprint(i)))
	}
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status?limit=2", f.admin, nil)
	rows := body["tasks"].([]any)
	if status != 200 || len(rows) != 2 || rows[0].(map[string]any)["taskIndex"] != float64(4) || rows[1].(map[string]any)["taskIndex"] != float64(3) || body["nextBeforeId"] != float64(3) || body["truncated"] != true || body["partial"] != false || body["pageComplete"] != true || body["hasMore"] != true {
		t.Fatal("latest page missing latest IDs", status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status?limit=2&beforeId=3", f.admin, nil)
	rows = body["tasks"].([]any)
	if status != 200 || len(rows) != 2 || rows[0].(map[string]any)["taskIndex"] != float64(2) || rows[1].(map[string]any)["taskIndex"] != float64(1) || body["nextBeforeId"] != nil || body["truncated"] != false {
		t.Fatal("earlier page duplicated or lost tasks", status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/tasks/1", f.admin, nil)
	if status != 200 || body["data"].(map[string]any)["taskIndex"] != float64(1) {
		t.Fatal("older task cannot be opened directly", status, body)
	}
	for _, p := range parents[1:] {
		stopAndRecyclePurgeFixture(t, f, p)
	}
	if _, err := f.master.tasks.repo.Purge(4); err != nil {
		t.Fatal(err)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status?limit=2", f.admin, nil)
	rows = body["tasks"].([]any)
	if status != 200 || len(rows) != 1 || rows[0].(map[string]any)["taskIndex"] != float64(1) || body["truncated"] != false {
		t.Fatal("deleted history consumed visible page slots", status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/deleted-tasks?limit=1", f.admin, nil)
	groups := body["deletedTasks"].(map[string]any)
	rows = groups["node.example"].([]any)
	if status != 200 || len(rows) != 1 || rows[0].(map[string]any)["taskIndex"] != float64(3) || body["nextBeforeId"] != float64(3) || body["partial"] != false || body["complete"] != true || body["pageComplete"] != true || body["hasMore"] != true {
		t.Fatal("recycle page included purged history", status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/deleted-tasks?limit=1&beforeId=3", f.admin, nil)
	rows = body["deletedTasks"].(map[string]any)["node.example"].([]any)
	if status != 200 || len(rows) != 1 || rows[0].(map[string]any)["taskIndex"] != float64(2) || body["truncated"] != false {
		t.Fatal("recycle earlier page wrong", status, body)
	}
	for _, query := range []string{"limit=0", "limit=10001", "beforeId=-1", "beforeId=abc"} {
		status, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status?"+query, f.admin, nil)
		if status != 400 {
			t.Fatal("invalid page accepted", query, status)
		}
	}
}

func TestTaskHTTPStatusAliasesPreserveRecordsAndGroupCounters(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for _, status := range []string{"PENDING", "WAITING", "RUNNING", "LIST_DONE", "STOPPING", "COMPLETED"} {
		p := persistAPITask(t, f, status)
		p.Status = status
		if err := f.master.store.Update(func(tx *state.Tx) error {
			return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), p)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, filter := range []string{"PENDING", "WAITING", "RUNNING", "LIST_DONE"} {
		code, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status?status="+filter, f.admin, nil)
		rows := body["tasks"].([]any)
		if code != 200 || len(rows) != 2 {
			t.Fatalf("%s: %d %#v", filter, code, body)
		}
		seen := map[string]bool{}
		for _, row := range rows {
			seen[row.(map[string]any)["status"].(string)] = true
		}
		metric, first, second := "runningTasks", "RUNNING", "LIST_DONE"
		if filter == "PENDING" || filter == "WAITING" {
			metric, first, second = "pendingTasks", "PENDING", "WAITING"
		}
		if !seen[first] || !seen[second] || body[metric] != float64(2) {
			t.Fatalf("aliases changed records or counters: %#v", body)
		}
	}
}
