package task

import (
	"context"
	"encoding/json"
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
	if count, err := r.ClearExceptions(context.Background(), filter); err != nil || count != 1 {
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
	if rows := exceptionRows(t, r, filter); len(rows) != 0 {
		t.Fatal("polling recreated hidden failure", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, r = openTest(t, path)
	defer db.Close()
	if err := r.EnsureExceptionIndex(); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 1 || rows[0].Node != "node-b" {
		t.Fatal("hidden state lost on restart", rows)
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
	if rows := exceptionRows(t, r, ExceptionFilter{}); len(rows) != 1 || rows[0].Attempt != 1 {
		t.Fatal("failed transaction damaged exception index", rows)
	}
	current, _ := r.Get(p.ID)
	if current.Counters != before.Counters {
		t.Fatal("failed index update partially changed counters")
	}
	records[0].Attempt = 2
	records[0].UpdatedAt = records[0].UpdatedAt.Add(time.Second)
	if err := r.Observe(p.ID, p.Generation, 1, records[0]); err != nil {
		t.Fatal(err)
	}
	if rows := exceptionRows(t, r, filter); len(rows) != 1 || rows[0].Attempt != 2 {
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
