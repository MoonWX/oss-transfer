package execution

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestExecutionTimeExcludesRetryQueueAndRestartDowntime(t *testing.T) {
	f := newFixture(t, 2)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f.repo.now = func() time.Time { return now }
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	r, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	start := *r.StartedAt
	now = now.Add(time.Minute)
	r, err = f.repo.finish(bg, f.job.ChildID, 1, r.Attempt, nil, ItemResult{ID: "1", Status: ItemSucceeded}, ItemResult{ID: "2", Status: ItemFailed, Error: "HTTP 400"})
	if err != nil || r.Status != PartialFailed {
		t.Fatal(r, err)
	}
	check := func(want time.Duration, running bool) Record {
		t.Helper()
		r, err := f.repo.Get(bg, f.job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		clock := r.ExecutionTime()
		if clock.Duration(now) != want || (clock.StartedAt != nil) != running {
			t.Fatalf("status=%s timing=%+v duration=%s want=%s running=%v", r.Status, clock, clock.Duration(now), want, running)
		}
		return r
	}
	now = now.Add(24 * time.Hour)
	check(time.Minute, false)
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false, true); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.resumeParent(bg, f.job.ParentID, 1, 2, true); err != nil {
		t.Fatal(err)
	}
	f.job.Generation = 2
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	check(time.Minute, false)
	r, err = f.repo.begin(bg, f.job.ChildID, 2, false, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	// A real result checkpoint proves the process executed through this point.
	r, err = f.repo.commit(bg, f.job.ChildID, 2, r.Attempt, []ItemResult{{ID: "2", Status: ItemSucceeded}})
	if err != nil {
		t.Fatal(err)
	}
	check(80*time.Second, true)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.repo.now = func() time.Time { return now }
	for range 2 {
		if err := f.repo.Recover(bg); err != nil {
			t.Fatal(err)
		}
		check(80*time.Second, false)
	}
	now = now.Add(time.Hour)
	r, err = f.repo.begin(bg, f.job.ChildID, 2, true, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	r, err = f.repo.finish(bg, f.job.ChildID, 2, r.Attempt, nil)
	if err != nil || r.Status != Completed {
		t.Fatal(r, err)
	}
	now = now.Add(24 * time.Hour)
	r = check(100*time.Second, false)
	if r.StartedAt == nil || !r.StartedAt.Equal(start) {
		t.Fatal("lost first start", r.StartedAt)
	}
}

func TestExecutionTimeFreezesForEveryChildTerminalState(t *testing.T) {
	for _, status := range []Status{Stopped, Failed, PartialFailed, Completed, Recoverable} {
		t.Run(string(status), func(t *testing.T) {
			f := newFixture(t, 2)
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			f.repo.now = func() time.Time { return now }
			if _, err := f.repo.Accept(bg, f.job); err != nil {
				t.Fatal(err)
			}
			r, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if status == Stopped {
				r, err = f.repo.stop(bg, f.job.ChildID, 1)
			} else {
				results := []ItemResult{{ID: "1", Status: ItemFailed}, {ID: "2", Status: ItemFailed}}
				if status == PartialFailed || status == Completed {
					results[0].Status = ItemSucceeded
				}
				if status == Completed {
					results[1].Status = ItemSucceeded
				}
				if status == Recoverable {
					results = nil
				}
				r, err = f.repo.finish(bg, f.job.ChildID, 1, r.Attempt, nil, results...)
			}
			if err != nil || r.Status != status || r.ExecutionTime().Duration(now.Add(24*time.Hour)) != time.Minute || r.ExecutionTime().StartedAt != nil {
				t.Fatal(r, err)
			}
		})
	}
}

func TestLegacyChildExecutionTimeRetainsKnownEnd(t *testing.T) {
	f := newFixture(t, 1)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	now := end.Add(24 * time.Hour)
	f.repo.now = func() time.Time { return now }
	r, err := f.repo.Accept(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	r.Status, r.StartedAt, r.FinishedAt, r.ActiveTime = Recoverable, &start, &end, nil
	if err := f.db.Update(func(tx *state.Tx) error { key, _ := jobKey(f.job.ChildID); return tx.Put(jobsBucket, key, r) }); err != nil {
		t.Fatal(err)
	}
	r, err = f.repo.begin(bg, f.job.ChildID, 1, true, false)
	if err != nil || r.ExecutionTime().Duration(now.Add(20*time.Second)) != 80*time.Second {
		t.Fatal(r, err)
	}
}

func TestParentRecoveryStopsLiveAttemptClockBeforeReadmission(t *testing.T) {
	f := newFixture(t, 2)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	f.repo.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.startEngine(nil, Options{WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		if first.CompareAndSwap(false, true) {
			return &blockingReader{ReadCloser: input, entered: entered, release: release, ctx: ctx}, nil
		}
		return input, nil
	}})
	f.acceptStart()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not start")
	}
	clock.Add(int64(time.Minute))
	if err := f.engine.ResumeParent(bg, f.job.ParentID, 2, true); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(24 * time.Hour))
	r, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || r.Status != Stopped || r.ExecutionTime().Duration(f.repo.now()) != time.Minute {
		t.Fatal(r, err)
	}
	f.job.Generation = 2
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	r, err = f.repo.Get(bg, f.job.ChildID)
	if err != nil || r.Status != Waiting || r.ExecutionTime().Duration(f.repo.now()) != time.Minute {
		t.Fatal(r, err)
	}
}
