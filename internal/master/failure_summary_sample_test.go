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

// Past failureSummaryScan records the summary is a sample of the lowest-
// numbered child tasks. Its message must say so and count records, not
// present sample counts next to the exact number of failed objects.
func TestFailureSummaryNamesItsSample(t *testing.T) {
	f := newAgentHTTPFixture(t)
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: "sample-node", EnrollmentID: "enrolled", Address: "sample.example"}}
	p, _, err := f.master.tasks.repo.Create(spec, "sampled-summary")
	if err != nil {
		t.Fatal(err)
	}
	// Two early children time out; seven later children are denied access.
	const children, perChild = 9, 128
	for number := 1; number <= children; number++ {
		objects := make([]transferstore.Object, perChild)
		for i := range objects {
			objects[i] = transferstore.Object{Key: fmt.Sprintf("%d/%03d", number, i), Size: 1}
		}
		c, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, objects)
		if err != nil {
			t.Fatal(err)
		}
		cause := "api error AccessDenied: denied"
		if number <= 2 {
			cause = "read tcp 10.0.0.1:443: i/o timeout"
		}
		record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: perChild, TotalBytes: perChild, Failed: perChild, FailedBytes: perChild}}
		for _, item := range c.Job.Items {
			record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: cause})
		}
		if err := f.master.tasks.repo.Observe(p.ID, p.Generation, number, record); err != nil {
			t.Fatal(err)
		}
	}
	// A last child succeeds, and the task ends partially failed.
	c, err := f.master.tasks.repo.Assign(p.ID, p.Generation, children+1, []transferstore.Object{{Key: "done", Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	done := execution.Record{Job: c.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1},
		Results: []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemSucceeded}}}
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, children+1, done); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.ScanProgress(p.ID, p.Generation, children*perChild+1, children*perChild+1, children+1, true, ""); err != nil {
		t.Fatal(err)
	}
	if ended, err := f.master.tasks.repo.FinishIfReady(p.ID, p.Generation); err != nil || !ended {
		t.Fatal("fixture task did not end", ended, err)
	}
	for _, test := range []struct{ language, want string }{
		{"zh-CN", "1152 个对象失败。按编号最小的子任务中的 1000 条失败记录统计，最多的原因（744 条）："},
		{"en-US", "1152 objects failed. Of 1000 failure records from the lowest-numbered subtasks, the most common cause (744 records): "},
		{"ja-JP", "1152 個のオブジェクトが失敗しました。番号の小さいサブタスクの失敗記録 1000 件のうち、最も多い原因（744 件）: "},
	} {
		code, _, body, err := localizedRequest(f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, test.language, nil)
		if err != nil || code != 200 {
			t.Fatal(code, body, err)
		}
		row := body["data"].(map[string]any)
		summary := row["failureSummary"].(map[string]any)
		if summary["truncated"] != true || summary["sampled"] != float64(failureSummaryScan) || summary["failed"] != float64(children*perChild) {
			t.Fatalf("%s: summary %+v", test.language, summary)
		}
		message, _ := summary["message"].(string)
		attention, _ := row["attention"].(map[string]any)
		if !strings.HasPrefix(message, test.want) || attention["message"] != message {
			t.Fatalf("%s: message %q, want prefix %q", test.language, message, test.want)
		}
	}
}
