package task

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func exceptionRows(t *testing.T, r *Repository, f ExceptionFilter) []Exception {
	t.Helper()
	rows := []Exception{}
	if err := r.VisitExceptions(context.Background(), f, func(e Exception) error { rows = append(rows, e); return nil }); err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestExceptionIndexAtomicityClearReplayRestartAndPurge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, r := openTest(t, path)
	if err := r.EnsureExceptionIndex(); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Nodes[0].Address = "node-a"
	spec.Nodes[1].Address = "node-b"
	p, _, err := r.Create(spec, "exception-parent")
	if err != nil {
		t.Fatal(err)
	}
	records := []execution.Record{}
	for index, name := range []string{"first", "second"} {
		child, err := r.Assign(p.ID, p.Generation, index+1, objects(name))
		if err != nil {
			t.Fatal(err)
		}
		record := execution.Record{Job: child.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC(), Results: []execution.ItemResult{{ID: child.Job.Items[0].ID, Status: execution.ItemFailed, Error: "destination permission denied"}}, Counters: execution.Counters{Total: 1, TotalBytes: 1024, Failed: 1, FailedBytes: 1024}}
		if err := r.Observe(p.ID, p.Generation, index+1, record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 2 {
		t.Fatal(rows)
	}
	filter := ExceptionFilter{ParentID: p.ID, Node: "node-a", SourceBucket: "bucket"}
	if count, err := r.ClearExceptions(context.Background(), filter); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	children, err := r.Children(p.ID, 0, 10)
	if err != nil || !reflect.DeepEqual(*children[0].Record, records[0]) {
		t.Fatal("clear mutated the execution ledger", err)
	}
	before, _ := r.Get(p.ID)
	if before.Counters.Failed != 2 {
		t.Fatal("clear changed task statistics")
	}
	records[0].UpdatedAt = records[0].UpdatedAt.Add(time.Second)
	if err := r.Observe(p.ID, p.Generation, 1, records[0]); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, filter); len(rows) != 1 {
		t.Fatal("unresolved failure was hidden", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, r = openTest(t, path)
	defer db.Close()
	if err := r.EnsureExceptionIndex(); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 2 {
		t.Fatal("unresolved failures lost on restart", rows)
	}
	// Fail after the old index has been replaced inside the transaction. Both
	// the old displayed entry and parent/child counters must roll back.
	malformed := records[1]
	malformed.Attempt = 2
	malformed.UpdatedAt = malformed.UpdatedAt.Add(time.Second)
	malformed.Results = append(append([]execution.ItemResult{}, malformed.Results...), execution.ItemResult{ID: "unknown", Status: execution.ItemFailed, Error: "bad ledger"})
	if err := r.Observe(p.ID, p.Generation, 2, malformed); err == nil {
		t.Fatal("unknown failure object accepted")
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 2 || rows[1].Attempt != 1 {
		t.Fatal("failed transaction damaged exception index", rows)
	}
	current, _ := r.Get(p.ID)
	if current.Counters != before.Counters {
		t.Fatal("failed index update partially changed counters")
	}
	records[0].Attempt = 2
	records[0].Status = execution.Running
	records[0].Results[0].Status = execution.ItemPending
	records[0].Results[0].Error = ""
	records[0].UpdatedAt = records[0].UpdatedAt.Add(time.Second)
	if err := r.Observe(p.ID, p.Generation, 1, records[0]); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, filter); len(rows) != 1 || rows[0].RetryStatus != ExceptionPending {
		t.Fatal("pending retry was hidden", rows)
	}
	records[0].Status = execution.Failed
	records[0].Results[0].Status = execution.ItemFailed
	records[0].Results[0].Error = "destination permission denied again"
	records[0].UpdatedAt = records[0].UpdatedAt.Add(time.Second)
	if err := r.Observe(p.ID, p.Generation, 1, records[0]); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, filter); len(rows) != 1 || rows[0].RetryCount != 1 || rows[0].RetryAttempt != 2 || rows[0].RetryStatus != ExceptionRetryFailed || rows[0].Error != "destination permission denied" {
		t.Fatal("new failed attempt remained hidden", rows)
	}
	if _, err := r.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recycle(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 0 {
		t.Fatal("recycled task visible in exceptions")
	}
	if _, err := r.Purge(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *state.Tx) error {
		for _, bucket := range []string{"master-task-exceptions", "master-task-exceptions-hidden"} {
			if err := tx.Scan(bucket, "", "", 100, func(string, json.RawMessage) error { t.Error("purge left exception metadata"); return nil }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExceptionIndexBackfillsExistingLedger(t *testing.T) {
	db, r := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _, err := r.Create(testSpec(), "backfill")
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Assign(p.ID, 1, 1, objects("old"))
	if err != nil {
		t.Fatal(err)
	}
	record := finished(c)
	record.Status = execution.Failed
	record.Results = []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemFailed, Error: "old error"}}
	c.Record = &record
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("master-task-children", childKey(p.ID, 1), c) }); err != nil {
		t.Fatal(err)
	}
	if len(exceptionRows(t, r, ExceptionFilter{})) != 0 {
		t.Fatal("fixture already indexed")
	}
	if err := r.EnsureExceptionIndex(); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 1 || rows[0].Error != "old error" {
		t.Fatal("existing ledger not indexed", rows)
	}
}

func TestExceptionClearOnlyResolvedOutcomesAndPreservesLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "resolved-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	items := append(objects("recovered"), objects("skipped")...)
	items = append(items, objects("failed")...)
	c, err := repo.Assign(p.ID, 1, 1, items)
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC()}
	for _, item := range c.Job.Items {
		record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "original failure"})
	}
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	record.Attempt++
	record.UpdatedAt = record.UpdatedAt.Add(time.Second)
	record.Results[0].Status, record.Results[0].Error = execution.ItemSucceeded, ""
	record.Results[1].Status, record.Results[1].Error = execution.ItemSkipped, ""
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	beforeParent, _ := repo.Get(p.ID)
	beforeChildren, _ := repo.Children(p.ID, 0, 10)
	for _, filter := range []ExceptionFilter{{ParentID: p.ID + 1}, {SourceBucket: "other"}, {DestinationBucket: "other"}, {Node: "other"}, {LocalOnly: true}} {
		if count, err := repo.ClearExceptions(context.Background(), filter); err != nil || count != 0 {
			t.Fatal("cleanup crossed its filter", filter, count, err)
		}
	}
	if count, err := repo.ClearExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}); err != nil || count != 2 {
		t.Fatal("resolved cleanup", count, err)
	}
	if count, err := repo.ClearExceptions(context.Background(), ExceptionFilter{}); err != nil || count != 0 {
		t.Fatal("cleanup was not idempotent", count, err)
	}
	afterParent, _ := repo.Get(p.ID)
	afterChildren, _ := repo.Children(p.ID, 0, 10)
	if !reflect.DeepEqual(beforeParent, afterParent) || !reflect.DeepEqual(beforeChildren, afterChildren) {
		t.Fatal("cleanup changed execution evidence")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	if rows := exceptionRows(t, repo, ExceptionFilter{}); len(rows) != 1 || rows[0].Key != "failed" {
		t.Fatal("resolved cleanup did not survive restart", rows)
	}
	// An unchanged report must stay hidden, but a new failure of that same
	// object must reappear even when its original error text is unchanged.
	record.UpdatedAt = record.UpdatedAt.Add(time.Second)
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{}); len(rows) != 1 {
		t.Fatal("report replay recreated resolved records", rows)
	}
	record.Attempt++
	record.UpdatedAt = record.UpdatedAt.Add(time.Second)
	record.Results[0].Status, record.Results[0].Error = execution.ItemFailed, "original failure"
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{}); len(rows) != 2 {
		t.Fatal("new failure remained hidden", rows)
	}
}

func TestLegacyHiddenUnresolvedExceptionsReappearBeforeRetryStateDerivation(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "legacy-hidden")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, objects("file"))
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC(), Results: []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemFailed, Error: "old failure"}}}
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	entry := exceptionRows(t, repo, ExceptionFilter{})[0]
	k := childKey(p.ID, 1) + "/" + digest(c.Job.Items[0].ID)
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("master-task-exceptions-hidden", k, entry.Identity) }); err != nil {
		t.Fatal(err)
	}
	check := func(status string) {
		t.Helper()
		rows := exceptionRows(t, repo, ExceptionFilter{})
		if len(rows) != 1 || rows[0].RetryStatus != status {
			t.Fatal("legacy hidden state suppressed unresolved work", status, rows)
		}
		if count, err := repo.ClearExceptions(context.Background(), ExceptionFilter{}); err != nil || count != 0 {
			t.Fatal("unresolved work was cleared", count, err)
		}
	}
	check(ExceptionNotRetried)
	p = resumeExceptionTest(t, repo, p)
	check(ExceptionPending)
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	check(ExceptionInterrupted)
}

func TestLegacyClearDoesNotHideLaterRecoveryOrCompletedTaskOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "legacy-clear-then-recover")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, append(objects("recover"), objects("skip")...))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC()
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: first, Counters: execution.Counters{Total: 2, TotalBytes: 2048, Failed: 2, FailedBytes: 2048}}
	for _, item := range c.Job.Items {
		record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemFailed, Error: "original upload error"})
	}
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	// Before resolved-only cleanup existed, clearing failures saved only the
	// original failure identity. It did not identify a resolved retry outcome.
	entries := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID})
	if err := db.Update(func(tx *state.Tx) error {
		for _, entry := range entries {
			for _, item := range c.Job.Items {
				if item.Source.Key == entry.Key {
					if err := tx.Put("master-task-exceptions-hidden", childKey(p.ID, 1)+"/"+digest(item.ID), entry.Identity); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p = resumeExceptionTest(t, repo, p)
	record.Job.Generation = p.Generation
	record.Attempt, record.Status, record.UpdatedAt = 2, execution.Completed, first.Add(time.Second)
	record.Results[0].Status, record.Results[0].Error = execution.ItemSucceeded, ""
	record.Results[1].Status, record.Results[1].Error = execution.ItemSkipped, ""
	record.Counters = execution.Counters{Total: 2, TotalBytes: 2048, Succeeded: 1, SucceededBytes: 1024, Skipped: 1, SkippedBytes: 1024}
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if err := repo.ScanProgress(p.ID, p.Generation, 2, 2048, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || !done {
		t.Fatal("task did not complete", done, err)
	}
	assertHistory := func() {
		t.Helper()
		rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID})
		if len(rows) != 2 {
			t.Fatal("legacy dismissal hid new retry results", rows)
		}
		for _, row := range rows {
			want := ExceptionRecovered
			if row.Key == "skip" {
				want = ExceptionSkipped
			}
			if row.RetryStatus != want || row.RetryGeneration != p.Generation || row.RetryCount != 1 || row.Error != "original upload error" || !row.ObservedAt.Equal(first) {
				t.Fatal("recovery history changed", row)
			}
		}
		options, more, err := repo.ExceptionTasks(context.Background(), ExceptionFilter{}, 100)
		if err != nil || more || len(options) != 1 || options[0].ID != p.ID || options[0].Status != "COMPLETED" {
			t.Fatal("completed task with uncleared history disappeared from picker", options, more, err)
		}
	}
	assertHistory()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	assertHistory()
	if count, err := repo.ClearExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}); err != nil || count != 2 {
		t.Fatal("explicit cleanup did not clear both resolved records", count, err)
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{}); len(rows) != 0 {
		t.Fatal("new resolved dismissal did not apply", rows)
	}
	if options, _, err := repo.ExceptionTasks(context.Background(), ExceptionFilter{}, 100); err != nil || len(options) != 0 {
		t.Fatal("cleaned task remained in exception picker", options, err)
	}
	parent, err := repo.Get(p.ID)
	if err != nil || parent.Status != "COMPLETED" || parent.Counters != record.Counters {
		t.Fatal("history cleanup altered the completed task", parent, err)
	}
}

func TestExceptionTaskOptionsAreBoundedAndExcludeUnrelatedTasks(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	for i := 1; i <= 105; i++ {
		spec := testSpec()
		if i%2 == 0 {
			spec.Direction = "LOCAL_TO_OSS"
			spec.Source = execution.Endpoint{Kind: "LOCAL", Root: "/data"}
		}
		p, _, err := repo.Create(spec, fmt.Sprintf("task-option-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if i == 105 { // A task without exceptions never appears.
			continue
		}
		c, err := repo.Assign(p.ID, 1, 1, objects("file"))
		if err != nil {
			t.Fatal(err)
		}
		record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC(), Results: []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemFailed, Error: "denied"}}}
		if err := repo.Observe(p.ID, 1, 1, record); err != nil {
			t.Fatal(err)
		}
		if i == 104 { // Entirely dismissed history is not a useful option.
			record.Results[0].Status, record.Results[0].Error = execution.ItemSucceeded, ""
			record.Attempt++
			record.UpdatedAt = record.UpdatedAt.Add(time.Second)
			if err := repo.Observe(p.ID, 1, 1, record); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.ClearExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}); err != nil {
				t.Fatal(err)
			}
		}
		if i == 103 {
			if _, err := repo.Stop(p.ID); err != nil {
				t.Fatal(err)
			}
			if err := repo.Stopped(p.ID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Recycle(p.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	parents, more, err := repo.ExceptionTasks(context.Background(), ExceptionFilter{}, 100)
	if err != nil || !more || len(parents) != 100 || parents[0].ID != 102 || parents[99].ID != 3 {
		t.Fatal("bounded options", len(parents), more, err)
	}
	parents, more, err = repo.ExceptionTasks(context.Background(), ExceptionFilter{ParentID: 1}, 100)
	if err != nil || more || len(parents) != 1 || parents[0].ID != 1 {
		t.Fatal("exact search could not reach older task", parents, more, err)
	}
	parents, more, err = repo.ExceptionTasks(context.Background(), ExceptionFilter{LocalOnly: true}, 100)
	if err != nil || more || len(parents) != 51 {
		t.Fatal("local options", len(parents), more, err)
	}
	for _, p := range parents {
		if p.Spec.Direction != "LOCAL_TO_OSS" {
			t.Fatal("unrelated direction", p.ID)
		}
	}
}
