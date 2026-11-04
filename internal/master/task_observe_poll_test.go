package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// pollAgent answers record reads with a real agentservice.Service over a real
// Agent database. mode "old" models a release without children_poll: it
// strips the protocol marker and fingerprints and rejects children_poll as an
// unknown operation. mode "tamper" answers notModified with a wrong
// fingerprint.
type pollAgent struct {
	t       *testing.T
	db      *state.Store
	service *agentservice.Service
	mu      sync.Mutex
	mode    string
	ops     map[string]int
	full    int // full records returned
}

func newPollAgent(t *testing.T) *pollAgent {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return nil, nil, errors.New("read only")
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()); _ = db.Close() })
	return &pollAgent{t: t, db: db, service: &agentservice.Service{Engine: engine}, ops: map[string]int{}}
}

func (a *pollAgent) put(child task.Child, record execution.Record) {
	a.t.Helper()
	record.Job = child.Job
	if err := a.db.Update(func(tx *state.Tx) error {
		return tx.Put("execution_jobs", fmt.Sprintf("%020d/%020d", child.Job.ParentID, child.Job.Number), record)
	}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *pollAgent) setMode(mode string) { a.mu.Lock(); a.mode = mode; a.mu.Unlock() }
func (a *pollAgent) counts() (map[string]int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ops := map[string]int{}
	for k, v := range a.ops {
		ops[k] = v
	}
	return ops, a.full
}

func (a *pollAgent) serve(ctx context.Context, broker *agenttransport.Broker, node task.Node) <-chan error {
	done := make(chan error, 1)
	go func() {
		for {
			command, err := broker.Poll(ctx, nodeSession(node))
			if err != nil {
				done <- err
				return
			}
			if err := broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
				done <- err
				return
			}
			a.mu.Lock()
			mode := a.mode
			a.ops[command.Operation]++
			a.mu.Unlock()
			var response agenttransport.Response
			switch {
			case command.Operation == "children_poll" && mode == "old":
				response = agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}
			case command.Operation == "children_get" || command.Operation == "children_poll":
				response, err = a.service.Execute(ctx, command)
				if err != nil {
					done <- err
					return
				}
				var page agentservice.ChildRecordsResponse
				if err := json.Unmarshal(response.Payload, &page); err != nil {
					done <- err
					return
				}
				for i := range page.Children {
					if mode == "old" {
						page.Children[i].Fingerprint = ""
					}
					if mode == "tamper" && page.Children[i].NotModified {
						page.Children[i].Fingerprint = "0" + page.Children[i].Fingerprint[1:]
					}
					if page.Children[i].Record != nil {
						a.mu.Lock()
						a.full++
						a.mu.Unlock()
					}
				}
				if mode == "old" {
					page.PollProtocol = 0
				}
				response.Payload, _ = json.Marshal(page)
			default:
				done <- fmt.Errorf("unexpected operation %s", command.Operation)
				return
			}
			if err := broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: response}); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}

type pollFixture struct {
	runtime  *taskRuntime
	p        task.Parent
	children []task.Child
	agent    *pollAgent
	now      time.Time
	cancel   context.CancelFunc
	done     <-chan error
}

func newPollFixture(t *testing.T, count int) *pollFixture {
	t.Helper()
	runtime, p, broker := schedulingFixture(t, 1)
	f := &pollFixture{runtime: runtime, p: p, agent: newPollAgent(t), now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	runtime.now = func() time.Time { return f.now }
	f.children = assignSchedulingChildren(t, runtime, p, count)
	for _, child := range f.children {
		f.agent.put(child, execution.Record{Attempt: 1, Status: execution.Running, UpdatedAt: f.now, Counters: execution.Counters{Total: 1, TotalBytes: 1}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, f.agent.serve(ctx, broker, p.Spec.Nodes[0])
	t.Cleanup(func() {
		cancel()
		if err := <-f.done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return f
}

func (f *pollFixture) observe(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.runtime.observeAvailable(ctx, f.p, 64, map[string]bool{f.p.Spec.Nodes[0].ID: true})
}

func (f *pollFixture) expect(t *testing.T, label string, ops map[string]int, full int) {
	t.Helper()
	gotOps, gotFull := f.agent.counts()
	for _, op := range []string{"children_get", "children_poll"} {
		if gotOps[op] != ops[op] {
			t.Fatalf("%s: %s calls = %d, want %d (all %v)", label, op, gotOps[op], ops[op], gotOps)
		}
	}
	if gotFull != full {
		t.Fatalf("%s: full records = %d, want %d", label, gotFull, full)
	}
}

// Unchanged records are confirmed by fingerprint, quiet children back off and
// a quiet task skips whole passes; a change is read in full and stored.
func TestObserveNotModifiedBacksOffAndRefetchesChangedRecords(t *testing.T) {
	f := newPollFixture(t, 20)
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	// 20 children in pages of 16: every record read in full, poll support learned.
	f.expect(t, "first pass", map[string]int{"children_get": 2}, 20)
	stored, err := f.runtime.repo.Child(f.p.ID, 3)
	if err != nil || stored.Record == nil || stored.Record.Attempt != 1 {
		t.Fatal("first pass not stored", stored.Record, err)
	}
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "second pass", map[string]int{"children_get": 2, "children_poll": 2}, 20)
	if !f.runtime.observeQuiet(f.p) {
		t.Fatal("a pass with every child unchanged did not back off")
	}
	// Not due yet: no Agent command at all.
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "backed off", map[string]int{"children_get": 2, "children_poll": 2}, 20)
	f.now = f.now.Add(observeBackoff(1))
	if f.runtime.observeQuiet(f.p) {
		t.Fatal("still quiet after the backoff elapsed")
	}
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "after backoff", map[string]int{"children_get": 2, "children_poll": 4}, 20)
	// The delay grows while nothing changes.
	f.now = f.now.Add(observeBackoff(1))
	if !f.runtime.observeQuiet(f.p) {
		t.Fatal("second quiet pass did not lengthen the backoff")
	}
	// Child 3 completes on the Agent.
	f.agent.put(f.children[2], execution.Record{Attempt: 1, Status: execution.Completed, UpdatedAt: f.now, Counters: execution.Counters{Total: 1, TotalBytes: 1, Succeeded: 1, SucceededBytes: 1}})
	f.now = f.now.Add(observeBackoff(2))
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "after change", map[string]int{"children_get": 2, "children_poll": 6}, 21)
	stored, err = f.runtime.repo.Child(f.p.ID, 3)
	if err != nil || stored.Record == nil || stored.Record.Status != execution.Completed {
		t.Fatal("changed record not stored", stored.Record, err)
	}
	p, err := f.runtime.repo.Get(f.p.ID)
	if err != nil || p.FinishedChildren != 1 || p.Counters.Succeeded != 1 {
		t.Fatal("parent counters missed the change", p.Counters, err)
	}
	if f.runtime.observeQuiet(f.p) {
		t.Fatal("a pass with a change still backed off")
	}
	// New children wake the observer immediately.
	f.now = f.now.Add(observePollMax)
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	if !f.runtime.observeQuiet(f.p) {
		t.Fatal("expected a quiet task")
	}
	f.runtime.wakeObserve(f.p.ID)
	if f.runtime.observeQuiet(f.p) {
		t.Fatal("wakeObserve did not end the backoff")
	}
}

// A new Master with an old Agent keeps reading full records; a downgraded
// Agent is detected and the same pass falls back to children_get.
func TestObservePollFallsBackForOldAndDowngradedAgents(t *testing.T) {
	t.Run("old-agent", func(t *testing.T) {
		f := newPollFixture(t, 4)
		f.agent.setMode("old")
		for pass := 1; pass <= 3; pass++ {
			if err := f.observe(t); err != nil {
				t.Fatal(err)
			}
			f.expect(t, fmt.Sprint("pass ", pass), map[string]int{"children_get": pass}, 4*pass)
			if f.runtime.observeQuiet(f.p) {
				t.Fatal("backed off without not-modified answers")
			}
		}
	})
	t.Run("downgraded-agent", func(t *testing.T) {
		f := newPollFixture(t, 4)
		if err := f.observe(t); err != nil {
			t.Fatal(err)
		}
		f.agent.setMode("old")
		if err := f.observe(t); err != nil {
			t.Fatal(err)
		}
		f.expect(t, "downgrade", map[string]int{"children_get": 2, "children_poll": 1}, 8)
		if f.runtime.nodePolls(f.p.Spec.Nodes[0]) {
			t.Fatal("poll support kept after unknown_operation")
		}
		if err := f.observe(t); err != nil {
			t.Fatal(err)
		}
		f.expect(t, "after downgrade", map[string]int{"children_get": 3, "children_poll": 1}, 12)
		// Upgrading again is learned from the next children_get response.
		f.agent.setMode("")
		for range 2 {
			if err := f.observe(t); err != nil {
				t.Fatal(err)
			}
		}
		f.expect(t, "upgrade", map[string]int{"children_get": 4, "children_poll": 2}, 16)
	})
}

// A notModified answer for a fingerprint the Master did not send is rejected,
// and fingerprints are discarded when the stored child no longer describes
// the record they identify.
func TestObservePollRejectsUnexpectedNotModifiedAndInvalidatesEntries(t *testing.T) {
	f := newPollFixture(t, 2)
	if err := f.observe(t); err != nil {
		t.Fatal(err)
	}
	f.agent.setMode("tamper")
	if err := f.observe(t); !errors.Is(err, task.ErrConflict) {
		t.Fatal("mismatched notModified accepted", err)
	}
	f.agent.setMode("")
	heads, err := f.runtime.repo.OutstandingHeads(f.p.ID, 0, 64)
	if err != nil || len(heads) != 2 {
		t.Fatal(len(heads), err)
	}
	// A failed pass recorded nothing new; the entries from the first pass are
	// still valid, so the next pass sends fingerprints again.
	due, known := f.runtime.observePlan(f.p, heads, 0, 1<<62)
	if len(due) != 2 || len(known) != 2 {
		t.Fatal("entries lost", len(due), known)
	}
	for _, change := range []func(*task.OutstandingChild, *task.Parent){
		func(h *task.OutstandingChild, _ *task.Parent) { h.RecordAttempt++ },
		func(h *task.OutstandingChild, _ *task.Parent) {
			h.RecordUpdatedAt = h.RecordUpdatedAt.Add(time.Nanosecond)
		},
		func(h *task.OutstandingChild, _ *task.Parent) { h.Child.Job.Assignment++ },
		func(h *task.OutstandingChild, _ *task.Parent) { h.Child.Node.EnrollmentID = "re-enrolled" },
		func(h *task.OutstandingChild, _ *task.Parent) { h.Child.Accepted = false },
		func(h *task.OutstandingChild, p *task.Parent) { h.RecordGeneration = p.Generation - 1 },
	} {
		changed := append([]task.OutstandingChild(nil), heads...)
		p := f.p
		change(&changed[0], &p)
		_, known := f.runtime.observePlan(p, changed, 0, 1<<62)
		if known[changed[0].Child.Job.Number] != "" {
			t.Fatal("stale fingerprint sent after the stored child changed")
		}
		// Rebuild the entry for the next case.
		if err := f.observe(t); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(observePollMax)
	}
	// A new generation discards every entry.
	resumed := f.p
	resumed.Generation++
	if _, known := f.runtime.observePlan(resumed, heads, 0, 1<<62); len(known) != 0 {
		t.Fatal("fingerprints survived a generation change", known)
	}
}

// TestObservePassMeasurement logs the before/after numbers for one quiet
// steady-state pass: the old path (an Agent without children_poll) versus the
// new one.
func TestObservePassMeasurement(t *testing.T) {
	for _, mode := range []string{"old", ""} {
		f := newPollFixture(t, 64)
		f.agent.setMode(mode)
		for range 2 {
			if err := f.observe(t); err != nil {
				t.Fatal(err)
			}
		}
		_, before := f.agent.counts()
		f.now = f.now.Add(observePollMax)
		if err := f.observe(t); err != nil {
			t.Fatal(err)
		}
		_, after := f.agent.counts()
		name := "children_poll"
		if mode == "old" {
			name = "children_get (old Agent)"
		}
		t.Logf("%s: full records sent per steady-state pass over 64 unchanged children: %d", name, after-before)
		if mode == "" && after != before {
			t.Fatal("unchanged records were resent")
		}
		if mode == "old" && after-before != 64 {
			t.Fatal("old path changed", after-before)
		}
	}
}
