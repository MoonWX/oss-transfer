package master

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A task node that holds no child of the task leaves no result unread: the
// report is complete even while that node is offline or removed.
func TestReportIgnoresTaskNodesWithoutChildren(t *testing.T) {
	g := newGenerationReport(t, "a", "b")
	spec := g.parent.Spec
	spec.Nodes = append(spec.Nodes, task.Node{ID: "idle-node", EnrollmentID: "idle-enrollment", Address: "idle.example"})
	p, _, err := g.f.master.tasks.repo.Create(spec, "idle-node-report")
	if err != nil {
		t.Fatal(err)
	}
	g.parent = p
	g.assign([]string{"a", "b"})
	if child := g.child(1); child.Node.ID == "idle-node" {
		t.Fatal("fixture placed the child on the idle node")
	}
	if record := g.run(1); record.Status != execution.Completed {
		t.Fatal("child did not complete", record.Status)
	}
	g.finish("COMPLETED")
	report := g.export()
	report.assertComplete(t, "a", "b")
	for _, row := range report.exceptions {
		if row["reasonCode"] == "NODE_REPORT_INCOMPLETE" {
			t.Fatal("an idle node was reported as holding unread results", row)
		}
	}
}

// The exception list is read from the Master's own index. It is incomplete
// only while an offline node owns unfinished children of an active task in
// the query's scope; idle, removed and finished nodes do not make it so.
func TestExceptionsIncompleteOnlyForOfflineNodesWithUnfinishedChildren(t *testing.T) {
	f := newAgentHTTPFixture(t)
	create := func(name string, nodes ...task.Node) task.Parent {
		t.Helper()
		spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
		if err != nil {
			t.Fatal(err)
		}
		spec.Nodes = nodes
		p, _, err := f.master.tasks.repo.Create(spec, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	observe := func(p task.Parent, number int, status execution.Status, failed bool) {
		t.Helper()
		c, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, []transferstore.Object{{Key: fmt.Sprintf("object-%d", number), Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		record := execution.Record{Job: c.Job, Status: status, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1}}
		result := execution.ItemResult{ID: c.Job.Items[0].ID, Status: execution.ItemPending}
		if failed {
			result.Status, result.Error = execution.ItemFailed, "api error AccessDenied: denied"
			record.Counters.Failed, record.Counters.FailedBytes = 1, 1
		}
		record.Results = []execution.ItemResult{result}
		if err := f.master.tasks.repo.Observe(p.ID, p.Generation, number, record); err != nil {
			t.Fatal(err)
		}
	}
	finished := task.Node{ID: "finished-node", EnrollmentID: "e1", Address: "finished.example"}
	busy := task.Node{ID: "busy-node", EnrollmentID: "e2", Address: "busy.example"}
	idle := task.Node{ID: "idle-node", EnrollmentID: "e3", Address: "idle.example"}
	// Every child of this task ended FAILED on an Agent that is now gone.
	ended := create("ended-task", finished)
	observe(ended, 1, execution.Failed, true)
	if err := f.master.tasks.repo.ScanProgress(ended.ID, ended.Generation, 1, 1, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if done, err := f.master.tasks.repo.FinishIfReady(ended.ID, ended.Generation); err != nil || !done {
		t.Fatal("fixture task did not end", done, err)
	}
	// A running task: one offline node still runs a child, another is idle.
	running := create("running-task", busy, idle)
	observe(running, 1, execution.Running, false)

	type expectation struct {
		filter     map[string]any
		unreported []string
	}
	for _, test := range []expectation{
		{map[string]any{"taskIndex": ended.ID}, nil},
		{map[string]any{"taskIndex": running.ID}, []string{"busy.example"}},
		{map[string]any{"taskIndex": running.ID, "workerNode": "idle.example"}, nil},
		{map[string]any{}, []string{"busy.example"}},
	} {
		code, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, "zh-CN", test.filter)
		if err != nil || code != 200 {
			t.Fatal(test.filter, code, body, err)
		}
		got := fmt.Sprint(body["unreachableWorkers"])
		if got != fmt.Sprint(test.unreported) && !(len(test.unreported) == 0 && got == "[]") || fmt.Sprint(body["failedWorkers"]) != got {
			t.Fatalf("%v: incomplete nodes %v %v, want %v", test.filter, body["unreachableWorkers"], body["failedWorkers"], test.unreported)
		}
		if (body["complete"] == true) != (len(test.unreported) == 0) || (body["partial"] == true) == (len(test.unreported) == 0) {
			t.Fatalf("%v: complete %v partial %v", test.filter, body["complete"], body["partial"])
		}
		warning, _ := body["warning"].(string)
		if len(test.unreported) > 0 && !strings.Contains(warning, "未完成子任务的失败记录可能尚未上报") || len(test.unreported) == 0 && warning != "" {
			t.Fatalf("%v: warning %q", test.filter, warning)
		}
		// Every node named is in the scope, with its own (possibly empty) group.
		groups := body["failedKeys"].(map[string]any)
		for _, node := range test.unreported {
			if _, found := groups[node]; !found || !strings.Contains(fmt.Sprint(body["queriedWorkers"]), node) {
				t.Fatalf("%v: %s missing from the scope: %v", test.filter, node, body)
			}
		}
	}
	// The ended task's failure is listed, and its gone Agent is not named.
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/failed", f.admin, map[string]any{"taskIndex": ended.ID})
	if code != 200 || body["totalCount"] != float64(1) {
		t.Fatal("ended task failure missing", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/local-transfers/failures?taskId=%d", f.http.URL, running.ID), f.admin, nil)
	data, _ := body["data"].(map[string]any)
	if code != 200 || data["complete"] != true || fmt.Sprint(data["unreachableWorkers"]) != "[]" {
		t.Fatal("local failures of an OSS task named its nodes", code, body)
	}
}
