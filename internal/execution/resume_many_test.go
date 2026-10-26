package execution

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestResumeParentDoesNotAdmitAllFailedHistory(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	const count = 1100
	var original Record
	if err := f.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(parentsBucket, parentKey(224), ParentFence{ParentID: 224, Generation: 1}); err != nil {
			return err
		}
		for n := 1; n <= count; n++ {
			job := f.job
			job.Number = n
			job.ChildID = fmt.Sprintf("224-%d", n)
			fingerprint, err := validate(job)
			if err != nil {
				return err
			}
			r := Record{Job: job, Status: PartialFailed, Attempt: 1, Fingerprint: fingerprint, Results: []ItemResult{{ID: "1", Status: ItemSucceeded}, {ID: "2", Status: ItemFailed, Error: "HTTP 400"}}}
			recount(&r)
			key, _ := jobKey(job.ChildID)
			if err := tx.Put(jobsBucket, key, r); err != nil {
				return err
			}
			if n == 1 {
				original = r
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatal("more failed children than capacity prevented recovery", err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatal("resume replay failed", err)
	}
	stale, err := f.repo.Get(bg, "224-1")
	if err != nil || !reflect.DeepEqual(stale, original) {
		t.Fatal("parent resume rewrote terminal ledgers", err)
	}
	if pending, _, err := f.repo.Unfinished(); err != nil || pending != 0 {
		t.Fatal(pending, err)
	}
	f.job.Generation = 2
	for n := 1; n <= 1024; n++ {
		job := f.job
		job.Number = n
		job.ChildID = fmt.Sprintf("224-%d", n)
		record, err := f.engine.Accept(bg, job)
		if err != nil || record.Results[0].Status != ItemSucceeded || record.Results[1].Status != ItemPending || record.Job.Generation != 2 {
			t.Fatal(n, record, err)
		}
	}
	job := f.job
	job.Number = 1025
	job.ChildID = "224-1025"
	if _, err := f.engine.Accept(bg, job); !errors.Is(err, ErrCapacity) {
		t.Fatal("admission stopped enforcing capacity", err)
	}
	// Finish one admitted child, then the next failed child can enter. No
	// successful result was erased and no full-parent transaction was required.
	first, err := f.repo.begin(bg, "224-1", 2, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.finish(context.Background(), "224-1", 2, first.Attempt, nil, ItemResult{ID: "2", Status: ItemSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Accept(bg, job); err != nil {
		t.Fatal("released capacity did not admit next retry", err)
	}
	unadmitted, err := f.repo.Get(bg, "224-1100")
	if err != nil || unadmitted.Job.Generation != 1 || unadmitted.Results[1].Status != ItemFailed {
		t.Fatal(unadmitted, err)
	}
}

func TestConcurrentParentRecoveryRetriesShareGeneration(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- f.engine.ResumeParent(bg, 224, 2, true) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("same recovery generation rejected concurrent replay", err)
		}
	}
	p, err := f.repo.Parent(bg, 224)
	if err != nil || p.Generation != 2 || p.Stopped {
		t.Fatal(p, err)
	}
	old, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || old.Job.Generation != 1 {
		t.Fatal("replayed recovery rewrote unadmitted work", old, err)
	}
}
