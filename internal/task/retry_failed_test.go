package task

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func outstandingChild(t *testing.T, repo *Repository, id int64, number int) bool {
	t.Helper()
	found := false
	if err := repo.store.View(func(tx *state.Tx) error {
		var value json.RawMessage
		var err error
		found, err = tx.Get("master-task-outstanding", childKey(id, number), &value)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// BeginRetry marks an ended child unfinished without touching its ledger. An
// Agent that never reopened it reports the same ended record, which restores
// the parent exactly; a reopened record moves the failed object to pending.
func TestBeginRetryKeepsLedgerUntilTheAgentReopensTheChild(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "running-retry")
	if err != nil {
		t.Fatal(err)
	}
	items := append(objects("ok"), objects("failed")...)
	c, err := repo.Assign(p.ID, p.Generation, 1, items)
	if err != nil {
		t.Fatal(err)
	}
	ended := execution.Record{Job: c.Job, Status: execution.PartialFailed, Attempt: 3, UpdatedAt: time.Now().UTC(),
		Results:  []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemSucceeded}, {ID: c.Job.Items[1].ID, Status: execution.ItemFailed, Error: "HTTP 503 SlowDown"}},
		Counters: execution.Counters{Total: 2, TotalBytes: 2048, Succeeded: 1, SucceededBytes: 1024, Failed: 1, FailedBytes: 1024}}
	if err := repo.Observe(p.ID, p.Generation, 1, ended); err != nil {
		t.Fatal(err)
	}
	// With the scan finished and dispatched, only unfinished children make a
	// node busy for the task's dynamic limit shares.
	if err := repo.ScanProgress(p.ID, p.Generation, 2, 2048, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	p, candidates, next, err := repo.RetryCandidates(p.ID, "", 64)
	if err != nil || next != "" || len(candidates) != 1 || candidates[0].Job.Number != 1 || p.FailedChildren != 1 || p.FinishedChildren != 1 {
		t.Fatal("ended failed child is not a candidate", candidates, next, err)
	}
	stale := candidates[0]
	staleRecord := *stale.Record
	staleRecord.Attempt--
	stale.Record = &staleRecord
	if marked, err := repo.BeginRetry(p.ID, p.Generation, []Child{stale}); err != nil || len(marked) != 0 {
		t.Fatal("an older observation was marked", marked, err)
	}
	marked, err := repo.BeginRetry(p.ID, p.Generation, candidates)
	if err != nil || len(marked) != 1 || marked[0].Record.Status != execution.Waiting {
		t.Fatal("candidate was not marked", marked, err)
	}
	if ShareBusy(p)[c.Node.ID] {
		t.Fatal("node with only ended children counted as busy")
	}
	p, err = repo.Get(p.ID)
	if err != nil || p.FinishedChildren != 0 || p.FailedChildren != 0 || p.Counters.Failed != 1 || p.Counters.Succeeded != 1 || p.Scheduling.Nodes[c.Node.ID].Children != 1 || !outstandingChild(t, repo, p.ID, 1) {
		t.Fatal("intent is not an unfinished, busy, observed child with an unchanged ledger", p, err)
	}
	if !ShareBusy(p)[c.Node.ID] {
		t.Fatal("a reopened child did not make its node busy")
	}
	if _, again, _, err := repo.RetryCandidates(p.ID, "", 64); err != nil || len(again) != 0 {
		t.Fatal("a marked child is offered again", again, err)
	}
	if finished, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || finished {
		t.Fatal("parent finished while the retry was pending", finished, err)
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); len(rows) != 1 || rows[0].RetryStatus != ExceptionNotRetried || rows[0].RetryCount != 0 {
		t.Fatal("intent was recorded as an object retry", rows)
	}
	// The Agent did not reopen it: the same ended record restores the parent.
	if err := repo.Observe(p.ID, p.Generation, 1, ended); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Get(p.ID)
	if err != nil || p.FinishedChildren != 1 || p.FailedChildren != 1 || p.Counters.Failed != 1 || p.Scheduling.Nodes[c.Node.ID].Children != 0 || outstandingChild(t, repo, p.ID, 1) {
		t.Fatal("unchanged Agent record did not restore the ledger", p, err)
	}
	if ShareBusy(p)[c.Node.ID] {
		t.Fatal("restored node stayed busy")
	}
	// A reopened record (pending failed object, next attempt) reopens the
	// child even when it arrives after the ended one.
	reopened := ended
	reopened.Status, reopened.Attempt, reopened.UpdatedAt = execution.Waiting, ended.Attempt+1, ended.UpdatedAt.Add(time.Second)
	reopened.Results = []execution.ItemResult{ended.Results[0], {ID: c.Job.Items[1].ID, Status: execution.ItemPending}}
	reopened.Counters = execution.Counters{Total: 2, TotalBytes: 2048, Succeeded: 1, SucceededBytes: 1024}
	if err := repo.Observe(p.ID, p.Generation, 1, reopened); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Get(p.ID)
	if err != nil || p.FinishedChildren != 0 || p.FailedChildren != 0 || p.Counters.Failed != 0 || p.Counters.Succeeded != 1 || !outstandingChild(t, repo, p.ID, 1) {
		t.Fatal("reopened record left an inexact ledger", p, err)
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); len(rows) != 1 || rows[0].RetryStatus != ExceptionPending || rows[0].RetryCount != 1 {
		t.Fatal("reopened object is not shown as retrying", rows)
	}
	// A stopping parent refuses new intents.
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	done := reopened
	done.Status, done.Attempt, done.UpdatedAt = execution.PartialFailed, reopened.Attempt+1, reopened.UpdatedAt.Add(time.Second)
	done.Results = ended.Results
	done.Counters = ended.Counters
	if err := repo.Observe(p.ID, p.Generation, 1, done); err != nil {
		t.Fatal(err)
	}
	_, candidates, _, err = repo.RetryCandidates(p.ID, "", 64)
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	if _, err := repo.BeginRetry(p.ID, p.Generation, candidates); err != ErrState {
		t.Fatal("stopping parent accepted a retry intent", err)
	}
}

// Candidates are found through the exception index in child order, past
// resolved records, and a page cursor continues exactly where a page ended.
func TestRetryCandidatesPageThroughTheExceptionIndex(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "running-retry-pages")
	if err != nil {
		t.Fatal(err)
	}
	failedRecord := func(c Child, attempt uint64) execution.Record {
		return execution.Record{Job: c.Job, Status: execution.Failed, Attempt: attempt, UpdatedAt: time.Now().UTC(),
			Results:  []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemFailed, Error: "HTTP 503 SlowDown"}},
			Counters: execution.Counters{Total: 1, TotalBytes: 1024, Failed: 1, FailedBytes: 1024}}
	}
	for number, key := range []string{"one", "two", "three"} {
		c, err := repo.Assign(p.ID, p.Generation, number+1, objects(key))
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Observe(p.ID, p.Generation, number+1, failedRecord(c, 1)); err != nil {
			t.Fatal(err)
		}
		if key == "two" {
			// Recovered: its exception record stays, resolved.
			done := finished(c)
			done.Attempt = 2
			done.Results = []execution.ItemResult{{ID: c.Job.Items[0].ID, Status: execution.ItemSucceeded, Target: c.Job.Items[0].Source}}
			if err := repo.Observe(p.ID, p.Generation, number+1, done); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rows := exceptionRows(t, repo, ExceptionFilter{ParentID: p.ID}); len(rows) != 3 || rows[1].RetryStatus != ExceptionRecovered {
		t.Fatal("fixture lacks a resolved record between the candidates", rows)
	}
	_, first, next, err := repo.RetryCandidates(p.ID, "", 1)
	if err != nil || len(first) != 1 || first[0].Job.Number != 1 || next == "" {
		t.Fatal("first page", first, next, err)
	}
	_, second, next, err := repo.RetryCandidates(p.ID, next, 1)
	if err != nil || len(second) != 1 || second[0].Job.Number != 3 || next != "" {
		t.Fatal("second page skipped a candidate or did not end", second, next, err)
	}
	if _, _, _, err := repo.RetryCandidates(p.ID, "00000000000000000999/", 1); err == nil {
		t.Fatal("cursor of another task accepted")
	}
}
