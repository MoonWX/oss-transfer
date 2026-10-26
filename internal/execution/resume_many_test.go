package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestResumeParentDoesNotAdmitAllFailedHistory(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	const count = 1100
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
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Compare persisted records on both sides of recovery. A round trip can
	// normalize time.Time's Location without changing any stored field.
	// Keep every ledger, so a rewrite outside the first child is also caught.
	snapshot := func() map[string][]byte {
		t.Helper()
		rows := map[string][]byte{}
		if err := f.db.View(func(tx *state.Tx) error {
			return tx.Scan(jobsBucket, "", "", count+1, func(key string, raw json.RawMessage) error {
				rows[key] = append([]byte(nil), raw...)
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	original := snapshot()
	if len(original) != count {
		t.Fatal("failed history fixture is incomplete", len(original))
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatal("more failed children than capacity prevented recovery", err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatal("resume replay failed", err)
	}
	stale := snapshot()
	if len(stale) != len(original) {
		t.Fatal("parent resume added or removed terminal ledgers", len(stale), len(original))
	}
	for key, before := range original {
		if !bytes.Equal(stale[key], before) {
			t.Fatal("parent resume rewrote terminal ledger", key)
		}
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
