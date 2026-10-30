package master

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopHTTPAcceptsWithoutWaitingAndSurvivesRestart(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, session := f.join(t, f.invite(t, "offline-stop.example"), "offline-stop", agentCredential(t))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	p := purgeFixtureTask(t, f, session, "stop-admission")
	// A previous scheduler step has not exited, and the Agent never polls.
	// Neither may hold the HTTP response after the stop intent is persisted.
	var cancellations atomic.Int64
	f.master.tasks.active[p.ID] = &taskRun{cancel: func() { cancellations.Add(1) }, done: make(chan struct{})}
	client := *f.http.Client()
	client.Timeout = time.Second
	for pass, endpoint := range []string{"stop", "local-transfers/stop"} {
		status, body := request(t, &client, "POST", f.http.URL+"/api/master/"+endpoint, f.admin, map[string]any{"taskIds": []int64{p.ID}})
		if data, ok := body["data"].(map[string]any); ok {
			body = data
		}
		accepted, ignored := float64(1), float64(0)
		if pass > 0 {
			accepted, ignored = 0, 1
		}
		if status != http.StatusOK || body["accepted"] != accepted || body["ignored"] != ignored || body["failed"] != float64(0) {
			t.Fatal(endpoint, status, body)
		}
		detail := body["details"].([]any)[0].(map[string]any)
		if detail["status"] != "STOPPING" {
			t.Fatal("acknowledgement claimed a completed stop", detail)
		}
	}
	if cancellations.Load() != 1 {
		t.Fatal("duplicate stop interrupted its own reconciliation", cancellations.Load())
	}
	delete(f.master.tasks.active, p.ID)
	before, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || before.Generation != p.Generation || before.BudgetRevision != p.BudgetRevision+1 {
		t.Fatal(before, err)
	}
	f.close()
	f.open(t)
	after, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || after.Status != "STOPPING" || after.Generation != before.Generation || after.BudgetRevision != before.BudgetRevision || after.Counters != before.Counters {
		t.Fatal("restart lost durable stop intent", after, err)
	}
	// The same reconciler resumes from durable STOPPING. Missing Agent ACKs
	// keep the task pending instead of changing it to STOPPED or losing data.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := f.master.tasks.step(ctx, p.ID); err == nil {
		t.Fatal("offline Agent falsely acknowledged stop")
	}
	after, err = f.master.tasks.repo.Get(p.ID)
	if err != nil || after.Status != "STOPPING" || after.Counters != before.Counters {
		t.Fatal(after, err)
	}
}

func TestCancelledStopDoesNotChangeTask(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, session := f.join(t, f.invite(t, "cancelled-stop.example"), "cancelled-stop", agentCredential(t))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	p := purgeFixtureTask(t, f, session, "cancelled-stop")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, admitted, err := f.master.requestTaskStop(ctx, p.ID); !errors.Is(err, context.Canceled) || admitted {
		t.Fatal("cancelled stop was admitted", admitted, err)
	}
	after, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || after.Status != p.Status || after.BudgetRevision != p.BudgetRevision {
		t.Fatal(after, err)
	}
}

func TestStopHTTPFinishesThroughBackgroundRuntime(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	p := takeoverParent(t, f, node)
	f.master.Start()
	client := *f.http.Client()
	client.Timeout = time.Second
	status, body := request(t, &client, "POST", f.http.URL+"/api/master/stop", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if status != http.StatusOK || body["accepted"] != float64(1) || body["failed"] != float64(0) {
		t.Fatal(status, body)
	}
	if detail := body["details"].([]any)[0].(map[string]any); detail["status"] != "STOPPING" {
		t.Fatal("HTTP waited for the final stop", detail)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		current, err := f.master.tasks.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "STOPPED" {
			if current.Generation != p.Generation || current.Counters != p.Counters {
				t.Fatal("background stop changed task data", current)
			}
			break
		}
		if current.Status != "STOPPING" {
			t.Fatal("cancelled old step overwrote stop intent", current.Status, current.Error, current.RuntimeError)
		}
		select {
		case <-deadline.C:
			t.Fatal("background runtime did not finish accepted stop", current.RuntimeError)
		case <-tick.C:
		}
	}
}
