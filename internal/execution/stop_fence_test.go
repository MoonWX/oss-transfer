package execution

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestStopParentPreservesCompletedHistoryAndFencesLaterPages(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	f.acceptStart()
	completed := f.wait()
	if completed.Status != Completed {
		t.Fatal(completed)
	}
	// More than two full pages of completed history precede the unfinished
	// children. A page containing only skipped records must not end the scan.
	if err := f.db.Update(func(tx *state.Tx) error {
		for number := 2; number <= 260; number++ {
			record := completed
			record.Job.Number, record.Job.ChildID = number, fmt.Sprintf("224-%d", number)
			key, _ := jobKey(record.Job.ChildID)
			if err := tx.Put(jobsBucket, key, record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.repo.now = func() time.Time { return start }
	states := []Status{Waiting, Waiting, Running, Recoverable, Failed, PartialFailed, Retired}
	before := map[string]Record{}
	for index, status := range states {
		job := f.job
		job.Number = 261 + index
		job.ChildID = fmt.Sprintf("224-%d", job.Number)
		if index == 1 {
			job.Items, job.DiscoveryComplete = nil, false // Not in the ready index.
		}
		if _, err := f.repo.Accept(bg, job); err != nil {
			t.Fatal(err)
		}
		record, err := f.repo.mutate(bg, job.ChildID, func(_ *state.Tx, r *Record) error {
			r.Status = status
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		before[job.ChildID] = record
	}
	f.repo.now = func() time.Time { return start.Add(time.Minute) }
	if err := f.engine.StopParent(bg, f.job.ParentID, 2); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"224-1", "224-128", "224-260"} {
		after, err := f.repo.Get(bg, id)
		want := completed
		want.Job.ChildID = id
		if _, err := fmt.Sscanf(id, "224-%d", &want.Job.Number); err != nil {
			t.Fatal(err)
		}
		if err != nil || !reflect.DeepEqual(after, want) {
			t.Fatalf("completed history was rewritten: %s %+v %v", id, after, err)
		}
	}
	for id, old := range before {
		after, err := f.repo.Get(bg, id)
		wantStatus := old.Status
		if !terminal(wantStatus) {
			wantStatus = Stopped
		}
		if err != nil || after.Status != wantStatus || after.Job.Generation != 2 || after.Attempt != old.Attempt+1 {
			t.Fatalf("unfinished or failed history was not fenced: %s %+v %v", id, after, err)
		}
		wantDuration := time.Duration(0)
		if old.Status == Running {
			wantDuration = time.Minute
		}
		if after.ExecutionTime().Duration(start.Add(24*time.Hour)) != wantDuration {
			t.Fatalf("stop changed execution timing: %s %+v", id, after.ActiveTime)
		}
		if _, err := f.repo.commit(bg, id, 1, old.Attempt, nil); !errors.Is(err, ErrGeneration) {
			t.Fatal("stale callback bypassed the parent fence", id, err)
		}
	}
	if _, err := f.repo.commit(bg, completed.Job.ChildID, 1, completed.Attempt, nil); !errors.Is(err, ErrGeneration) {
		t.Fatal("unchanged completed generation bypassed the parent fence", err)
	}
	if pending, err := f.repo.Pending(bg, "", 256); err != nil || len(pending) != 0 {
		t.Fatal("stopped jobs remained runnable", pending, err)
	}
	if running, err := f.repo.running(bg, "", 256); err != nil || len(running) != 0 {
		t.Fatal("stopped jobs remained in the running index", running, err)
	}
	// Reconciliation must not deserialize hundreds of completed ledgers again.
	// Leave a wide allocation margin for Go/bbolt bookkeeping, independent of
	// filesystem speed; the old full-history stop needs thousands of allocations.
	allocations := testing.AllocsPerRun(3, func() {
		if err := f.repo.fenceParent(bg, f.job.ParentID, 2, false); err != nil {
			t.Fatal(err)
		}
	})
	if allocations > 500 {
		t.Fatalf("repeated stop reread history: %.0f allocations", allocations)
	}
}

func TestStopParentFenceOnlyDoesNotSkipLedgerFence(t *testing.T) {
	f := newFixture(t, 1)
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false, true); err != nil {
		t.Fatal(err)
	}
	p, err := f.repo.Parent(bg, f.job.ParentID)
	if err != nil || !p.Stopped || p.StopJobsFenced {
		t.Fatal("fence-only phase incorrectly claimed complete stop", p, err)
	}
	queued, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || queued.Status != Waiting {
		t.Fatal(queued, err)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false); err != nil {
		t.Fatal(err)
	}
	p, _ = f.repo.Parent(bg, f.job.ParentID)
	stopped, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || !p.StopJobsFenced || stopped.Status != Stopped {
		t.Fatal("full stop skipped queued child after fence-only", p, stopped, err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.repo.Parent(bg, f.job.ParentID)
	if !p.StopJobsFenced {
		t.Fatal("restart lost completed stop fence", p)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false); err != nil {
		t.Fatal(err)
	}
	after, _ := f.repo.Get(bg, f.job.ChildID)
	if !reflect.DeepEqual(after, stopped) {
		t.Fatal("replayed stop changed its ledger", stopped, after)
	}
	if err := f.repo.resumeParent(bg, f.job.ParentID, 1, 2, true); err != nil {
		t.Fatal(err)
	}
	p, _ = f.repo.Parent(bg, f.job.ParentID)
	if p.Stopped || p.StopJobsFenced {
		t.Fatal("resume retained a completed stop fence", p)
	}
	f.job.Generation = 2
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 2, false); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 3, false); err != nil {
		t.Fatal(err)
	}
	after, _ = f.repo.Get(bg, f.job.ChildID)
	if after.Status != Stopped || after.Job.Generation != 3 {
		t.Fatal("higher-generation stop reused old completed fence", after)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 2, false); !errors.Is(err, ErrGeneration) {
		t.Fatal("old stop bypassed generation fence", err)
	}
}

func TestStopParentFenceCompletionIsAtomic(t *testing.T) {
	f := newFixture(t, 1)
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	observed := &observedDB{db: f.db, child: f.job.ChildID}
	f.repo.db = observed
	observed.failAll.Store(true)
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false); err == nil {
		t.Fatal("stop committed despite database failure")
	}
	p, _ := f.repo.Parent(bg, f.job.ParentID)
	record, _ := f.repo.Get(bg, f.job.ChildID)
	if p.Stopped || p.StopJobsFenced || record.Status != Waiting {
		t.Fatal("failed transaction left a partial stop", p, record)
	}
	observed.failAll.Store(false)
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false); err != nil {
		t.Fatal(err)
	}
	p, _ = f.repo.Parent(bg, f.job.ParentID)
	record, _ = f.repo.Get(bg, f.job.ChildID)
	if !p.Stopped || !p.StopJobsFenced || record.Status != Stopped {
		t.Fatal("failed stop could not be retried", p, record)
	}
}
