package task

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// The exception index keeps why an object failed and why its retry was
// skipped, and the Master accepts the current version a retry pinned for a
// source that changed after discovery.
func TestExceptionReasonsForChangedAndDeletedSources(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "source-reasons")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, p.Generation, 1, append(objects("changed"), objects("deleted")...))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC()
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: first, Counters: execution.Counters{Total: 2, TotalBytes: 2048, Failed: 2, FailedBytes: 2048}}
	for _, item := range c.Job.Items {
		record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "source changed", Reconciliation: execution.ReasonSourceChanged})
	}
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	for _, row := range exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}) {
		if row.Reason != execution.ReasonSourceChanged || row.RetryReason != "" {
			t.Fatalf("original failure reason: %+v", row)
		}
	}
	p = resumeExceptionTest(t, repo, p)
	current := transferstore.Object{Key: "changed", Size: 4096, ETag: "current"}
	retried := execution.Record{Job: c.Job, Status: execution.Completed, Attempt: 4, UpdatedAt: first.Add(time.Second), Counters: execution.Counters{Total: 2, TotalBytes: 1024 + 4096, Succeeded: 1, SucceededBytes: 4096, Skipped: 1, SkippedBytes: 1024}}
	retried.Job.Generation = p.Generation
	retried.Results = []execution.ItemResult{
		{ID: c.Job.Items[0].ID, Status: execution.ItemSucceeded, Target: transferstore.Object{Key: "changed", Size: 4096}, ResolvedSource: &current, Reconciliation: execution.ReasonSourceRefreshed},
		{ID: c.Job.Items[1].ID, Status: execution.ItemSkipped, Target: transferstore.Object{Key: "deleted"}, Reconciliation: execution.ReasonSourceDeleted},
	}
	if err := repo.Observe(p.ID, p.Generation, 1, retried); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{"changed": {ExceptionRecovered, ""}, "deleted": {ExceptionSkipped, execution.ReasonSourceDeleted}}
	for _, row := range exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}) {
		if row.Reason != execution.ReasonSourceChanged || row.RetryStatus != want[row.Key][0] || row.RetryReason != want[row.Key][1] {
			t.Fatalf("retry outcome of %s: %+v", row.Key, row)
		}
		if row.Key == "changed" && row.Size != 4096 {
			t.Fatalf("exception did not take the current size: %+v", row)
		}
	}
	saved, err := repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Counters != retried.Counters {
		t.Fatalf("parent counters %+v, want %+v", saved.Counters, retried.Counters)
	}
}
