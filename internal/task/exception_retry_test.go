package task

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func resumeExceptionTest(t *testing.T, repo *Repository, p Parent) Parent {
	t.Helper()
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, accepted, err := repo.RequestResume(p.ID, &p.Generation)
	if err != nil || !accepted {
		t.Fatal("resume was not accepted", p, err)
	}
	for {
		ready, err := repo.PrepareResume(p.ID, p.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
	}
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExceptionRetryOutcomesArePerObjectDurableAndIgnoreAttemptFences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "object-retry-outcomes")
	if err != nil {
		t.Fatal(err)
	}
	items := append(objects("recover"), objects("fail-again")...)
	items = append(items, objects("skip")...)
	items = append(items, objects("normal")...)
	c, err := repo.Assign(p.ID, p.Generation, 1, items)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC()
	record := execution.Record{Job: c.Job, Status: execution.PartialFailed, Attempt: 7, UpdatedAt: first}
	for _, item := range c.Job.Items {
		status := execution.ItemFailed
		if item.Source.Key == "normal" {
			status = execution.ItemSucceeded
		}
		record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: status, Error: "original upload error"})
	}
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	initial := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID})
	if len(initial) != 3 {
		t.Fatal("normal objects entered exception history", initial)
	}
	// Stop/restart fencing can advance Attempt without retrying any object.
	record.Status, record.Attempt, record.UpdatedAt = execution.Stopped, 9, first.Add(time.Second)
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if got := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); !reflect.DeepEqual(initial, got) {
		t.Fatal("fencing was counted as object retries", got)
	}
	p = resumeExceptionTest(t, repo, p)
	for _, row := range exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}) {
		if row.RetryStatus != ExceptionPending || row.RetryCount != 0 || row.RetryGeneration != p.Generation {
			t.Fatal("accepted request pretended to have an object result", row)
		}
	}
	old := record
	record.Job.Generation, record.Attempt, record.Status = p.Generation, 12, execution.PartialFailed
	record.UpdatedAt = first.Add(2 * time.Second)
	// Deliberately omit the intermediate PENDING poll. Fast retries can finish
	// before the Master polls, with different outcomes inside the same child.
	record.Results = append([]execution.ItemResult(nil), record.Results...)
	record.Results[0].Status, record.Results[0].Error = execution.ItemSucceeded, ""
	record.Results[1].Error = "new permission error"
	record.Results[2].Status, record.Results[2].Error = execution.ItemSkipped, ""
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID})
	want := map[string]string{"recover": ExceptionRecovered, "fail-again": ExceptionRetryFailed, "skip": ExceptionSkipped}
	for _, row := range rows {
		if row.RetryStatus != want[row.Key] || row.RetryCount != 1 || row.Error != "original upload error" || !row.ObservedAt.Equal(first) || !row.RetryUpdatedAt.Equal(record.UpdatedAt) {
			t.Fatal("object retry outcome or original evidence is wrong", row)
		}
		if row.Key == "fail-again" && row.RetryError != "new permission error" {
			t.Fatal(row)
		}
	}
	if err := repo.Observe(p.ID, p.Generation, 1, old); !errors.Is(err, ErrConflict) {
		t.Fatal("old generation observation accepted", err)
	}
	// A new snapshot caused by a different object must not rewrite timestamps
	// or invent another retry for this object's unchanged terminal result.
	record.UpdatedAt = first.Add(3 * time.Second)
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if got := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); !reflect.DeepEqual(rows, got) {
		t.Fatal("unchanged results rewrote retry history", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	if got := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); !reflect.DeepEqual(rows, got) {
		t.Fatal("retry outcomes lost on restart", got)
	}
	// Another parent retry only concerns the still-failed object. Successful
	// and skipped objects keep their original retry count and outcome.
	p = resumeExceptionTest(t, repo, p)
	record.Job.Generation, record.Attempt = p.Generation, 15
	record.UpdatedAt = first.Add(4 * time.Second)
	record.Results[1].Status, record.Results[1].Error = execution.ItemPending, ""
	record.Status = execution.Running
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	for _, row := range exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}) {
		if row.Key == "fail-again" {
			if row.RetryStatus != ExceptionInterrupted || row.RetryCount != 2 {
				t.Fatal("stopped retry stayed running", row)
			}
		} else if row.RetryStatus != want[row.Key] || row.RetryCount != 1 {
			t.Fatal("other objects were retried", row)
		}
	}
}

func TestExceptionRetryOldIndexAssignmentAndReadOnlyQuery(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "old-exception-index")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, p.Generation, 1, objects("old-failure"))
	if err != nil {
		t.Fatal(err)
	}
	c.Job.Assignment = 4
	record := execution.Record{Job: c.Job, Status: execution.Failed, Attempt: 7, UpdatedAt: time.Now().UTC(), Results: []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemFailed, Error: "old failure"}}}
	c.Record = &record
	entry := Exception{ParentID: p.ID, ChildID: c.Job.ChildID, Generation: 1, Attempt: 7, Key: "old-failure", Error: "old failure", ObservedAt: record.UpdatedAt, Identity: "old-hidden-identity"}
	k := childKey(p.ID, 1) + "/" + digest(c.Job.Items[0].ID)
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put("master-task-children", childKey(p.ID, 1), c); err != nil {
			return err
		}
		return tx.Put("master-task-exceptions", k, entry)
	}); err != nil {
		t.Fatal(err)
	}
	record.UpdatedAt = record.UpdatedAt.Add(time.Second)
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID})
	if len(rows) != 1 || rows[0].RetryCount != 0 || rows[0].RetryStatus != ExceptionNotRetried || rows[0].Assignment != 4 || rows[0].Identity != entry.Identity {
		t.Fatal("old assignment was counted as a retry", rows)
	}
	var before, after json.RawMessage
	read := func(into *json.RawMessage) error {
		return db.View(func(tx *state.Tx) error { _, err := tx.Get("master-task-exceptions", k, into); return err })
	}
	if err := read(&before); err != nil {
		t.Fatal(err)
	}
	// Even a parent marked COMPLETED cannot prove an unobserved object retry.
	if err := db.Update(func(tx *state.Tx) error {
		p.Generation, p.Status = 2, "COMPLETED"
		return repo.save(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.VisitExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}, func(e Exception) error {
		if e.RetryStatus != ExceptionInterrupted || e.RetryCount != 0 {
			t.Fatal("parent completion invented recovery", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := read(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("query status wrote to exception storage")
	}
}
