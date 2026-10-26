package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestTaskControlHTTPLargeSelectionPersistsEveryResultAndKeepsRetryGuards(t *testing.T) {
	f := newAgentHTTPFixture(t)
	code, session := f.join(t, f.invite(t, "bulk.example"), "bulk-control", agentCredential(t))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	ids := make([]int64, 140)
	guards := map[int64]uint64{}
	for index := range ids {
		p := purgeFixtureTask(t, f, session, fmt.Sprintf("bulk-%d", index))
		if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
			t.Fatal(err)
		}
		ids[len(ids)-index-1], guards[p.ID] = p.ID, p.Generation
	}
	// Validate the whole selection before admitting any task, even after the
	// former 128-item boundary. An invalid last entry must not partly execute.
	duplicate := append(append([]int64(nil), ids...), ids[0])
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/retry", f.admin, map[string]any{"taskIds": duplicate})
	if code != http.StatusBadRequest {
		t.Fatal("duplicate accepted", code, body)
	}
	for _, id := range ids {
		p, err := f.master.tasks.repo.Get(id)
		if err != nil || p.Generation != 1 || p.Status != "STOPPED" {
			t.Fatal("invalid request partly executed", p, err)
		}
	}
	const missingID = int64(999999)
	ids = append(ids, missingID)
	guards[missingID] = 1
	payload := map[string]any{"taskIds": ids, "expectedGenerations": guards}
	for pass := range 2 {
		code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/retry", f.admin, payload)
		wantAccepted, wantIgnored := float64(140), float64(0)
		if pass == 1 {
			wantAccepted, wantIgnored = 0, 140
		}
		if code != http.StatusOK || body["accepted"] != wantAccepted || body["ignored"] != wantIgnored || body["failed"] != float64(1) || body["partial"] != true {
			t.Fatal("large batch result", pass, code, body)
		}
		details := body["details"].([]any)
		if len(details) != len(ids) {
			t.Fatal("missing per-task results", len(details), len(ids))
		}
		for index, id := range ids {
			item := details[index].(map[string]any)
			if item["taskId"] != float64(id) || item["success"] != (id != missingID) {
				t.Fatal("result order or success mismatch", index, item)
			}
			if id == missingID {
				if item["message"] == "" {
					t.Fatal("failed task has no reason", item)
				}
				continue
			}
			p, err := f.master.tasks.repo.Get(id)
			if err != nil || p.Generation != 2 || p.Status != "RESUMING" || item["generation"] != float64(2) {
				t.Fatal("resume not durably admitted exactly once", p, item, err)
			}
		}
	}
}

func TestTaskControlBudgetIsSharedAcrossRequestsAndCancellationSkipsQueuedTasks(t *testing.T) {
	s := &Server{taskControlSlots: make(chan struct{}, taskControlConcurrency)}
	ids := make([]int64, 140)
	for index := range ids {
		ids[index] = int64(index + 1)
	}
	var active, maximum, calls atomic.Int64
	started := make(chan int64, len(ids)*2)
	release := make(chan struct{})
	control := func(ctx context.Context, id int64) (task.Parent, bool, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for prior := maximum.Load(); current > prior && !maximum.CompareAndSwap(prior, current); prior = maximum.Load() {
		}
		calls.Add(1)
		started <- id
		<-release
		return task.Parent{ID: id}, true, nil
	}
	firstDone := make(chan []taskControlOutcome, 1)
	go func() { firstDone <- s.runTaskControls(context.Background(), ids, control) }()
	for range taskControlConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("task controls did not run concurrently")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondDone := make(chan []taskControlOutcome, 1)
	go func() { secondDone <- s.runTaskControls(ctx, ids, control) }()
	select {
	case id := <-started:
		close(release)
		t.Fatalf("second request exceeded shared budget: task %d", id)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case results := <-secondDone:
		for _, result := range results {
			if !errors.Is(result.err, context.Canceled) || result.admitted {
				t.Fatal("queued cancelled task was started", result)
			}
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("cancelled request waited for another request's slots")
	}
	close(release)
	select {
	case results := <-firstDone:
		for index, result := range results {
			if result.err != nil || !result.admitted || result.parent.ID != ids[index] {
				t.Fatal("result order or accepted task lost", index, result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("remaining admitted request did not finish")
	}
	if maximum.Load() != taskControlConcurrency || calls.Load() != int64(len(ids)) || len(s.taskControlSlots) != 0 {
		t.Fatal("budget or cancellation mismatch", maximum.Load(), calls.Load(), len(s.taskControlSlots))
	}
}

func TestCancelledRetryDoesNotAdvanceTaskGeneration(t *testing.T) {
	f := newAgentHTTPFixture(t)
	code, session := f.join(t, f.invite(t, "cancelled.example"), "cancelled-control", agentCredential(t))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	p := purgeFixtureTask(t, f, session, "cancelled-retry")
	if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, admitted, err := f.master.requestTaskResume(ctx, p.ID, &p.Generation); !errors.Is(err, context.Canceled) || admitted {
		t.Fatal("cancelled retry admitted", admitted, err)
	}
	after, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || after.Generation != 1 || after.Status != "STOPPED" {
		t.Fatal("cancelled retry changed task", after, err)
	}
}
