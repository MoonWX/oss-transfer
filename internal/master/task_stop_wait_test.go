package master

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestStopWaitOnlyClassifiesTimeouts(t *testing.T) {
	timeout := fmt.Errorf("Agent stop: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"local deadline", timeout, true},
		{"remote deadline", &AgentError{Status: 408, Message: "context deadline exceeded"}, true},
		{"joined deadlines", fmt.Errorf("stop: %w", errors.Join(timeout, timeout)), true},
		{"mixed disk failure", errors.Join(timeout, errors.New("disk full")), false},
		{"mixed generation failure", errors.Join(timeout, task.ErrGeneration), false},
		{"session lost", agenttransport.ErrSession, false},
		{"canceled round", context.Canceled, false},
		{"untyped message", errors.New("context deadline exceeded"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyStopTimeouts(tc.err); got != tc.want {
				t.Fatalf("onlyStopTimeouts(%v)=%v", tc.err, got)
			}
		})
	}
}

// Use real Agent transport and stop persistence, injecting only an unconfirmed
// response. The Master must keep reconciling instead of either declaring STOPPED
// or presenting the first timeout as a migration failure.
type stopWaitAgent struct {
	*agentservice.Service
	mode  atomic.Int64
	calls atomic.Int64
}

func (a *stopWaitAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	mode := a.mode.Load()
	if mode != 0 && (c.Operation == "parent_stop" || c.Operation == "scan_stop") {
		a.calls.Add(1)
		if mode == 2 && c.Operation == "parent_stop" {
			return agenttransport.Response{Status: 500, Payload: []byte(`{"message":"disk full"}`)}, nil
		}
		return agenttransport.Response{Status: 408, Payload: []byte(`{"message":"context deadline exceeded"}`)}, nil
	}
	return a.Service.Execute(ctx, c)
}

func TestStopWaitReconcilesWithoutTransientErrorAndExposesPersistentFaults(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f.master.options.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, "slow-stop.example"), "slow-stop-agent", credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	a := &stopWaitAgent{Service: service}
	a.mode.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	runner := agenttransport.NewRunner(agentClient(t, f, credential), a, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		f.master.tasks.close()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Agent did not exit")
		}
		_ = service.Close(context.Background())
		_ = db.Close()
	})
	node := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "slow-stop.example"}
	p := taskLimitsStatusParent(t, f, node)
	p, _, err = f.master.tasks.repo.RequestStop(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.RuntimeProblem(p.ID, p.Generation, "old timeout"); err != nil {
		t.Fatal(err)
	}
	f.master.Start()
	waitFor := func(description string, check func(task.Parent) bool) task.Parent {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			current, err := f.master.tasks.repo.Get(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if check(current) {
				return current
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: status=%s error=%s", description, current.Status, current.RuntimeError)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("short timeout remains pending without an error", func(p task.Parent) bool {
		return a.calls.Load() >= 4 && p.Status == "STOPPING" && p.RuntimeError == ""
	})
	a.mode.Store(2)
	waitFor("mixed real failure remains visible inside the grace period", func(p task.Parent) bool {
		return p.Status == "STOPPING" && strings.Contains(p.RuntimeError, "disk full")
	})
	a.mode.Store(1)
	waitFor("a resolved hard error clears during short acknowledgement waits", func(p task.Parent) bool {
		return p.Status == "STOPPING" && p.RuntimeError == ""
	})
	clock.Add(int64(stopAcknowledgementGrace + time.Second))
	waitFor("persistent timeout remains visible", func(p task.Parent) bool {
		return p.Status == "STOPPING" && strings.Contains(p.RuntimeError, "context deadline exceeded")
	})
	a.mode.Store(0)
	stopped := waitFor("confirmed stop clears the error", func(p task.Parent) bool {
		return p.Status == "STOPPED" && p.RuntimeError == ""
	})
	if stopped.Counters.Failed != 0 {
		t.Fatal("control timeout changed failed object count", stopped.Counters)
	}
	fence, err := service.Jobs.Parent(context.Background(), p.ID)
	if err != nil || !fence.Stopped || fence.Generation != p.Generation {
		t.Fatal("stop completed without Agent fence", fence, err)
	}
}

func TestStopWaitScopeAndCleanup(t *testing.T) {
	now := time.Now()
	runtime := &taskRuntime{server: &Server{options: Options{Now: func() time.Time { return now }}}}
	p := task.Parent{ID: 1, Generation: 1, BudgetRevision: 1, Status: "STOPPING"}
	timeout := context.DeadlineExceeded
	if !isStopAwaitingAcknowledgement(runtime.stopControlProblem(p, timeout)) {
		t.Fatal("initial timeout was not pending")
	}
	now = now.Add(stopAcknowledgementGrace)
	if isStopAwaitingAcknowledgement(runtime.stopControlProblem(p, timeout)) {
		t.Fatal("persistent timeout remained hidden")
	}
	p.BudgetRevision++
	if isStopAwaitingAcknowledgement(runtime.stopControlProblem(p, timeout)) {
		t.Fatal("editing limits restarted grace for the same stop")
	}
	p.Generation++ // Another stop after an explicit resume.
	if !isStopAwaitingAcknowledgement(runtime.stopControlProblem(p, timeout)) {
		t.Fatal("new stop inherited an expired grace period")
	}
	p.Status = "RUNNING"
	if isStopAwaitingAcknowledgement(runtime.stopControlProblem(p, timeout)) {
		t.Fatal("non-stop timeout was hidden")
	}
	runtime.forgetStopObservation(p.ID)
	if len(runtime.stopWaits) != 0 {
		t.Fatal("finished task retained stop waiting state")
	}
}
