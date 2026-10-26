package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func prepareResume(t *testing.T, repo *Repository, p Parent) {
	t.Helper()
	for {
		ready, err := repo.PrepareResume(p.ID, p.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
	}
}

func TestResumeIsDurableBoundedAndKeepsSuccessfulHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { db.Close() }()
	p, _, err := repo.Create(testSpec(), "retry-many")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a real large terminal ledger in one fixture transaction. Its layout
	// is the production layout; test recovery uses real durable transactions.
	const count = 1100
	var completedRaw json.RawMessage
	err = db.Update(func(tx *state.Tx) error {
		p.Status = "PARTIAL_FAILED"
		p.ChildCount = count
		p.FinishedChildren = count
		p.FailedChildren = count - 1
		p.ScannedBatches = count
		p.DispatchedThrough = count
		p.ScanComplete = true
		p.Counters = execution.Counters{Total: count, TotalBytes: count * 1024, Succeeded: 1, SucceededBytes: 1024, Failed: count - 1, FailedBytes: (count - 1) * 1024}
		for n := 1; n <= count; n++ {
			job := execution.Job{ChildID: fmt.Sprintf("%d-%d", p.ID, n), ParentID: p.ID, Number: n, Generation: 1, DiscoveryComplete: true}
			child := Child{Job: job, Node: p.Spec.Nodes[0], Accepted: true, Record: &execution.Record{Job: job, Status: execution.Failed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1024, Failed: 1, FailedBytes: 1024}}}
			if n == 1 {
				child.Record.Status = execution.Completed
				child.Record.Counters = execution.Counters{Total: 1, TotalBytes: 1024, Succeeded: 1, SucceededBytes: 1024}
				completedRaw, _ = json.Marshal(child)
			}
			if err := tx.Put("master-task-children", childKey(p.ID, n), child); err != nil {
				return err
			}
		}
		p.ChildOrder = &ChildOrderState{Version: 1}
		return save(tx, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil || p.Generation != 2 || p.Status != "RESUMING" || p.ResumeAfter == nil || *p.ResumeAfter != 0 {
		t.Fatal(p, err)
	}
	same, err := repo.Resume(p.ID)
	if err != nil || same.Generation != p.Generation {
		t.Fatal("duplicate accepted new generation", same, err)
	}
	untouched, _ := repo.Children(p.ID, 1099, 1)
	if untouched[0].Job.Generation != 1 {
		t.Fatal("HTTP acceptance rewrote history")
	}
	if err := repo.Resumed(p.ID, p.Generation); !errors.Is(err, ErrState) {
		t.Fatal("started incomplete recovery", err)
	}
	ready, err := repo.PrepareResume(p.ID, p.Generation)
	if err != nil || ready {
		t.Fatal(ready, err)
	}
	current, _ := repo.Get(p.ID)
	if current.ResumeAfter == nil || *current.ResumeAfter != 128 || current.FinishedChildren != count-127 {
		t.Fatal(current)
	}
	db.Close()
	db, repo = openTest(t, path)
	prepareResume(t, repo, current)
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	current, _ = repo.Get(p.ID)
	if current.FinishedChildren != 1 || current.FailedChildren != 0 || current.Counters.Succeeded != 1 || current.Counters.Failed != count-1 {
		t.Fatal(current)
	}
	var completed Child
	if err := db.View(func(tx *state.Tx) error {
		_, err := tx.Get("master-task-children", childKey(p.ID, 1), &completed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(completed)
	if !reflect.DeepEqual(actual, []byte(completedRaw)) {
		t.Fatal("completed assignment was rewritten")
	}
	rows, err := repo.Outstanding(p.ID, 0, 256)
	if err != nil || len(rows) != 256 || rows[0].Job.Number != 2 || rows[0].Job.Generation != 2 {
		t.Fatal(rows, err)
	}
	if _, err := repo.PrepareResume(p.ID, 1); !errors.Is(err, ErrGeneration) {
		t.Fatal("stale page bypassed generation", err)
	}
}

func TestResumeUpgradesAlreadyAcceptedOldRecovery(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "old-resume")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, objects("object"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		p, err = get(tx, p.ID)
		if err != nil {
			return err
		}
		p.Status = "RESUMING"
		p.Generation = 2
		p.Scheduling = nil
		c.Job.Generation = 2
		c.Record = &execution.Record{Job: c.Job, Status: execution.Waiting}
		if err := tx.Put("master-task-children", childKey(p.ID, 1), c); err != nil {
			return err
		}
		return save(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil || p.Generation != 2 || p.ResumeAfter == nil {
		t.Fatal(p, err)
	}
	prepareResume(t, repo, p)
	if err := repo.Resumed(p.ID, 2); err != nil {
		t.Fatal(err)
	}
	current, _ := repo.Get(p.ID)
	if current.Scheduling == nil || current.Scheduling.Nodes[c.Node.ID].Objects != 1 {
		t.Fatal(current)
	}
}
