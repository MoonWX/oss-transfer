package task

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestExecutionTimePausesAcrossRetryAndMasterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	start := now
	repo.now = func() time.Time { return now }
	p, _, err := repo.Create(testSpec(), "timed")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ScanProgress(p.ID, 1, 2, 20, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "one", Size: 10}, {Key: "two", Size: 10}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	rec := execution.Record{Job: c.Job, Status: execution.PartialFailed, Attempt: 1, UpdatedAt: now, Counters: execution.Counters{Total: 2, TotalBytes: 20, Succeeded: 1, SucceededBytes: 10, Failed: 1, FailedBytes: 10}}
	if err := repo.Observe(p.ID, 1, 1, rec); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || !done {
		t.Fatal(done, err)
	}
	check := func(want time.Duration, running bool) Parent {
		t.Helper()
		p, err := repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		clock := p.ExecutionTime()
		if clock.Duration(now) != want || (clock.StartedAt != nil) != running {
			t.Fatalf("status=%s timing=%+v duration=%s want=%s running=%v", p.Status, clock, clock.Duration(now), want, running)
		}
		return p
	}
	now = now.Add(24 * time.Hour)
	check(time.Minute, false)
	p, err = repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation := p.Generation
	now = now.Add(5 * time.Minute)
	p, err = repo.Resume(p.ID)
	if err != nil || p.Generation != generation {
		t.Fatal(p, err)
	}
	check(time.Minute, false)
	prepareResume(t, repo, p)
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := repo.RuntimeProblem(p.ID, p.Generation, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	check(70*time.Second, true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	repo.now = func() time.Time { return now }
	now = now.Add(20 * time.Second)
	check(90*time.Second, true)
	children, err := repo.Children(p.ID, 0, 10)
	if err != nil || len(children) != 1 {
		t.Fatal(children, err)
	}
	rec.Job, rec.Status, rec.Attempt, rec.UpdatedAt = children[0].Job, execution.Completed, 2, now
	rec.Counters = execution.Counters{Total: 2, TotalBytes: 20, Succeeded: 2, SucceededBytes: 20}
	if err := repo.Observe(p.ID, p.Generation, 1, rec); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || !done {
		t.Fatal(done, err)
	}
	now = now.Add(24 * time.Hour)
	p = check(90*time.Second, false)
	if p.StartedAt == nil || !p.StartedAt.Equal(start) {
		t.Fatal("lost first start", p.StartedAt)
	}
}

func TestExecutionTimeFreezesForEveryTerminalState(t *testing.T) {
	for _, status := range []string{"STOPPED", "FAILED", "PARTIAL_FAILED", "COMPLETED"} {
		t.Run(status, func(t *testing.T) {
			db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
			defer db.Close()
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			repo.now = func() time.Time { return now }
			p, _, err := repo.Create(testSpec(), status)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if status == "STOPPED" {
				if _, err := repo.Stop(p.ID); err != nil {
					t.Fatal(err)
				}
				if err := repo.Stopped(p.ID, p.Generation); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.Update(func(tx *state.Tx) error {
					current, err := get(tx, p.ID)
					if err != nil {
						return err
					}
					current.ScanComplete = true
					if status != "COMPLETED" {
						current.Error = "scan failed"
					}
					if status == "PARTIAL_FAILED" {
						current.Counters.Succeeded = 1
					}
					return repo.save(tx, current)
				}); err != nil {
					t.Fatal(err)
				}
				if done, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || !done {
					t.Fatal(done, err)
				}
			}
			p, err = repo.Get(p.ID)
			if err != nil || p.Status != status || p.ExecutionTime().Duration(now.Add(48*time.Hour)) != time.Minute || p.ExecutionTime().StartedAt != nil {
				t.Fatal(p, err)
			}
		})
	}
}

func TestLegacyExecutionTimeKeepsKnownIntervalOnResume(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	finish := start.Add(3 * time.Minute)
	now := finish.Add(48 * time.Hour)
	repo.now = func() time.Time { return now }
	p, _, err := repo.Create(testSpec(), "legacy-timing")
	if err != nil {
		t.Fatal(err)
	}
	p.Status, p.StartedAt, p.FinishedAt, p.ActiveTime = "PARTIAL_FAILED", &start, &finish, nil
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("master-tasks", key(p.ID), p) }); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	prepareResume(t, repo, p)
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	p, err = repo.Get(p.ID)
	if err != nil || p.ExecutionTime().Duration(now) != 200*time.Second {
		t.Fatal(p, err)
	}
}

func TestOldAcceptedResumeRecoversTimingInBoundedPages(t *testing.T) {
	for _, hasEnd := range []bool{true, false} {
		t.Run(map[bool]string{true: "child_end_available", false: "history_incomplete"}[hasEnd], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.db")
			db, repo := openTest(t, path)
			defer func() { _ = db.Close() }()
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			now := start.Add(72 * time.Hour)
			repo.now = func() time.Time { return now }
			p, _, err := repo.Create(testSpec(), "old-resume-timing")
			if err != nil {
				t.Fatal(err)
			}
			p.Status, p.Generation, p.StartedAt, p.FinishedAt, p.ActiveTime = "RESUMING", 2, &start, nil, nil
			p.ChildCount, p.FinishedChildren, p.ScannedBatches, p.DispatchedThrough = 130, 130, 130, 130
			p.ChildOrder = &ChildOrderState{Version: 1}
			if err := db.Update(func(tx *state.Tx) error {
				for number := 1; number <= p.ChildCount; number++ {
					job := execution.Job{ParentID: p.ID, Number: number, Generation: 2, ChildID: fmt.Sprintf("%d-%d", p.ID, number)}
					rec := &execution.Record{Job: job, Status: execution.Completed, StartedAt: &start}
					rec.Job.Generation = 1
					if hasEnd {
						end := start.Add(time.Hour)
						if number == 130 {
							end = start.Add(4 * time.Hour)
						}
						rec.FinishedAt = &end
					}
					if err := tx.Put("master-task-children", childKey(p.ID, number), Child{Job: job, Node: p.Spec.Nodes[0], Record: rec}); err != nil {
						return err
					}
				}
				return tx.Put("master-tasks", key(p.ID), p)
			}); err != nil {
				t.Fatal(err)
			}
			// Runtime/index updates before the control step must not hide the old
			// schema and fabricate elapsed time from the seventy-two hour wait.
			if err := repo.RuntimeProblem(p.ID, p.Generation, "agent offline"); err != nil {
				t.Fatal(err)
			}
			p, err = repo.Resume(p.ID)
			if err != nil || p.ResumeTiming == nil || p.Generation != 2 {
				t.Fatal(p, err)
			}
			if ready, err := repo.PrepareResume(p.ID, 2); err != nil || ready {
				t.Fatal(ready, err)
			}
			p, err = repo.Get(p.ID)
			if err != nil || p.ResumeAfter == nil || *p.ResumeAfter != 128 || p.ResumeTiming == nil {
				t.Fatal(p, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, repo = openTest(t, path)
			repo.now = func() time.Time { return now }
			now = now.Add(24 * time.Hour)
			prepareResume(t, repo, p)
			p, err = repo.Get(p.ID)
			if err != nil || p.ResumeTiming != nil {
				t.Fatal(p, err)
			}
			want := time.Duration(0)
			if hasEnd {
				want = 4 * time.Hour
			}
			if p.ExecutionTime().Duration(now) != want || p.ActiveTime.Incomplete == hasEnd || p.ActiveTime.Estimated != hasEnd {
				t.Fatal(p.ActiveTime)
			}
			if err := repo.Resumed(p.ID, 2); err != nil {
				t.Fatal(err)
			}
			p, err = repo.Get(p.ID)
			if err != nil || p.ExecutionTime().Duration(now.Add(20*time.Second)) != want+20*time.Second {
				t.Fatal(p.ActiveTime, err)
			}
		})
	}
}
