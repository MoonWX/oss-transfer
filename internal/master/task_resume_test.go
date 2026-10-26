package master

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestRetryHTTPPersistsAcceptanceWithoutWaitingForOfflineAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, session := f.join(t, f.invite(t, "offline.example"), "offline-retry", agentCredential(t))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	p := purgeFixtureTask(t, f, session, "retry-offline")
	if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	// The Agent never polls. The HTTP call must still return its durable
	// acceptance; waiting for any command acknowledgement would exceed this.
	client := *f.http.Client()
	client.Timeout = time.Second
	for index, route := range []string{"retry", "resume", "local-transfers/retry-failures"} {
		status, body := request(t, &client, "POST", f.http.URL+"/api/master/"+route, f.admin, map[string]any{"taskIds": []int64{p.ID}})
		if data, ok := body["data"].(map[string]any); ok {
			body = data
		}
		wantAccepted, wantIgnored := float64(0), float64(1)
		if index == 0 {
			wantAccepted, wantIgnored = 1, 0
		}
		if status != http.StatusOK || body["accepted"] != wantAccepted || body["ignored"] != wantIgnored || body["failed"] != float64(0) {
			t.Fatal(route, status, body)
		}
		detail := body["details"].([]any)[0].(map[string]any)
		if detail["status"] != "RESUMING" {
			t.Fatal(detail)
		}
		current, err := f.master.tasks.repo.Get(p.ID)
		if err != nil || current.Generation != 2 || current.Status != "RESUMING" {
			t.Fatal(current, err)
		}
	}
	// Preparation is durable independently of the request's cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.master.tasks.step(ctx, p.ID); err == nil {
		t.Fatal("cancelled preparation continued")
	}
	current, _ := f.master.tasks.repo.Get(p.ID)
	if current.ResumeAfter == nil {
		t.Fatal("cancelled step consumed recovery cursor")
	}
	f.close()
	f.open(t)
	restored, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || restored.Status != "RESUMING" || restored.Generation != 2 || restored.ResumeAfter == nil {
		t.Fatal(restored, err)
	}
}

// Concurrent clicks from different pages and delayed responses share the task's
// durable generation, rather than an in-memory debounce window.
func TestRetryHTTPConcurrentAndDelayedClicksOnlyAcceptOneGeneration(t *testing.T) {
	f := newAgentHTTPFixture(t)
	code, session := f.join(t, f.invite(t, "retry.example"), "duplicate-retry", agentCredential(t))
	if code != http.StatusOK {
		t.Fatal(code)
	}
	p := purgeFixtureTask(t, f, session, "duplicate-retry")
	if err := f.master.tasks.repo.ScanProgress(p.ID, p.Generation, 2, 2, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	child, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "done", Size: 1}, {Key: "failed", Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	finishPartial := func(generation uint64, attempt uint64) {
		t.Helper()
		children, err := f.master.tasks.repo.Children(p.ID, 0, 1)
		if err != nil || len(children) != 1 {
			t.Fatal(children, err)
		}
		record := execution.Record{Job: children[0].Job, Status: execution.PartialFailed, Attempt: attempt, UpdatedAt: time.Now(), Counters: execution.Counters{Total: 2, TotalBytes: 2, Succeeded: 1, SucceededBytes: 1, Failed: 1, FailedBytes: 1}}
		if err := f.master.tasks.repo.Observe(p.ID, generation, 1, record); err != nil {
			t.Fatal(err)
		}
		if done, err := f.master.tasks.repo.FinishIfReady(p.ID, generation); err != nil || !done {
			t.Fatal(done, err)
		}
	}
	finishPartial(p.Generation, 1)
	code, detail := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil)
	if code != 200 || detail["data"].(map[string]any)["generation"] != float64(1) {
		t.Fatal(code, detail)
	}
	payload := map[string]any{"taskIds": []int64{p.ID}, "expectedGenerations": map[int64]uint64{p.ID: 1}}
	routes := []string{"retry", "resume", "start", "retry-task", "local-transfers/resume", "local-transfers/retry-failures"}
	type response struct {
		code int
		body map[string]any
	}
	responses := make(chan response, 24)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/"+routes[i%len(routes)], f.admin, payload)
			responses <- response{code, body}
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	accepted, ignored, count := 0., 0., 0
	for result := range responses {
		body := result.body
		if data, ok := body["data"].(map[string]any); ok {
			body = data
		}
		if result.code != 200 || body["failed"] != float64(0) {
			t.Fatal(result.code, body)
		}
		item := body["details"].([]any)[0].(map[string]any)
		if item["generation"] != float64(2) || item["status"] != "RESUMING" || item["success"] != true {
			t.Fatal(item)
		}
		accepted += body["accepted"].(float64)
		ignored += body["ignored"].(float64)
		count++
	}
	if count != 24 || accepted != 1 || ignored != 23 {
		t.Fatal(count, accepted, ignored)
	}
	if ready, err := f.master.tasks.repo.PrepareResume(p.ID, 2); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if err := f.master.tasks.repo.Resumed(p.ID, 2); err != nil {
		t.Fatal(err)
	}
	// Even callers without a generation must not reset a running task.
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/resume", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if code != 200 || body["ignored"] != float64(1) || body["failed"] != float64(0) {
		t.Fatal(code, body)
	}
	finishPartial(2, 2)
	before, _ := f.master.tasks.repo.Get(p.ID)
	f.close()
	f.open(t)
	// The first attempt has already failed again before the delayed duplicate
	// reaches Master. It must not start generation 3, including after restart.
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/retry", f.admin, payload)
	if code != 200 || body["ignored"] != float64(1) || body["accepted"] != float64(0) || body["failed"] != float64(0) {
		t.Fatal(code, body)
	}
	after, _ := f.master.tasks.repo.Get(p.ID)
	if after.Generation != 2 || after.Status != "PARTIAL_FAILED" || after.Counters != before.Counters || after.ChildCount != 1 {
		t.Fatal(after)
	}
	children, _ := f.master.tasks.repo.Children(p.ID, 0, 1)
	if children[0].Job.ChildID != child.Job.ChildID || children[0].Record.Attempt != 2 {
		t.Fatal(children)
	}
	payload["expectedGenerations"] = map[int64]uint64{p.ID: 2}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/retry", f.admin, payload)
	if code != 200 || body["accepted"] != float64(1) || body["details"].([]any)[0].(map[string]any)["generation"] != float64(3) {
		t.Fatal(code, body)
	}
}

func TestRetryHTTPRejectsIncompleteGenerationGuards(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for _, guards := range []map[int64]uint64{{}, {1: 0}, {2: 1}, {1: 1, 2: 1}} {
		code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/retry", f.admin, map[string]any{"taskIds": []int64{1}, "expectedGenerations": guards})
		if code != http.StatusBadRequest {
			t.Fatal(code, body)
		}
	}
}
