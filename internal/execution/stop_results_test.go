package execution

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// stopGateStore writes most objects at once and holds the write of one key
// until its context ends, counting the writes of each key.
type stopGateStore struct {
	transferstore.Store
	mu      sync.Mutex
	hold    string
	writes  map[string]int
	held    chan struct{}
	written chan struct{}
}

func (s *stopGateStore) Write(ctx context.Context, key string, reader io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.mu.Lock()
	s.writes[key]++
	hold := key == s.hold
	s.mu.Unlock()
	if hold {
		s.held <- struct{}{}
		<-ctx.Done()
		return transferstore.WriteResult{}, ctx.Err()
	}
	result, err := s.Store.Write(ctx, key, reader, opts)
	s.written <- struct{}{}
	return result, err
}

func awaitSignals(t *testing.T, signal <-chan struct{}, count int, what string) {
	t.Helper()
	for range count {
		select {
		case <-signal:
		case <-time.After(time.Minute): // hang guard only
			t.Fatalf("never reached: %s", what)
		}
	}
}

// Objects an attempt completed before a stop, a shutdown or an interrupted
// lease are recorded, however recently they finished: they are at the
// destination. An object the interruption cut off stays pending, never
// failed. Work moved to another Agent records nothing, as before.
func TestInterruptedAttemptRecordsCompletedObjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trigger  func(f *fixture, lease context.CancelCauseFunc) error
		status   Status
		recorded bool
	}{
		{"stop child", func(f *fixture, _ context.CancelCauseFunc) error { return f.engine.Stop(bg, f.job.ChildID, 1) }, Stopped, true},
		{"stop task", func(f *fixture, _ context.CancelCauseFunc) error { return f.engine.StopParent(bg, f.job.ParentID, 1) }, Stopped, true},
		{"fence before resume", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.ResumeParent(bg, f.job.ParentID, 2, false)
		}, Stopped, true},
		{"shutdown", func(f *fixture, _ context.CancelCauseFunc) error { return f.engine.Close(bg) }, Recoverable, true},
		{"lease expired", func(_ *fixture, lease context.CancelCauseFunc) error {
			lease(transferstore.ErrInterrupted)
			return nil
		}, Recoverable, true},
		{"quiesce for a new lease", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.QuiesceParent(bg, f.job.ParentID, transferstore.ErrInterrupted)
		}, Recoverable, true},
		{"moved to another Agent", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.QuiesceParent(bg, f.job.ParentID, transferstore.ErrMoved)
		}, Recoverable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 3)
			store := &stopGateStore{hold: f.job.Items[2].DestinationKey, writes: map[string]int{}, held: make(chan struct{}, 1), written: make(chan struct{}, 8)}
			base := f.factory()
			lease, cancelLease := context.WithCancelCause(bg)
			defer cancelLease(nil)
			// No periodic commit: completed results wait in the batch.
			f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
				src, dst, err := base.Open(ctx, job)
				store.mu.Lock()
				store.Store = dst
				store.mu.Unlock()
				return src, store, err
			}), Options{WorkersPerJob: 4, CommitInterval: time.Hour, JobContext: func(Job) (context.Context, error) { return lease, nil }})
			f.acceptStart()
			awaitSignals(t, store.held, 1, "the held write")
			awaitSignals(t, store.written, 2, "the completed writes")
			if err := tc.trigger(f, cancelLease); err != nil {
				t.Fatal(err)
			}
			if tc.name != "shutdown" {
				if _, err := f.engine.Wait(bg, f.job.ChildID); err != nil {
					t.Fatal(err)
				}
			}
			record, err := f.repo.Get(bg, f.job.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			want := []ItemStatus{ItemPending, ItemPending, ItemPending}
			if tc.recorded {
				want[0], want[1] = ItemSucceeded, ItemSucceeded
			}
			for i, result := range record.Results {
				if result.Status != want[i] {
					t.Fatalf("object %d is %s, want %s (%+v)", i, result.Status, want[i], record)
				}
			}
			if record.Status != tc.status || record.Counters.Failed != 0 {
				t.Fatalf("record %s with %d failed, want %s: %+v", record.Status, record.Counters.Failed, tc.status, record.Counters)
			}
			if tc.name != "stop child" {
				return
			}
			// Continuing transfers only the object that was cut off.
			store.mu.Lock()
			store.hold = ""
			store.mu.Unlock()
			if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
				t.Fatal(err)
			}
			awaitSignals(t, store.written, 1, "the resumed write")
			if record := f.wait(); record.Status != Completed {
				t.Fatal(record)
			}
			f.assertFiles()
			store.mu.Lock()
			defer store.mu.Unlock()
			for i, item := range f.job.Items {
				if want := map[bool]int{true: 2, false: 1}[i == 2]; store.writes[item.DestinationKey] != want {
					t.Fatalf("object %d written %d times, want %d", i, store.writes[item.DestinationKey], want)
				}
			}
		})
	}
}

// commitAfterStop records only succeeded and skipped objects that are still
// pending, and only for the stop of the attempt that completed them.
func TestCommitAfterStopRecordsOnlyThatStopsCompletedObjects(t *testing.T) {
	f := newFixture(t, 4)
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	running, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(i int) string { return f.job.Items[i].ID }
	if _, err := f.repo.commitAfterStop(bg, f.job.ChildID, 1, running.Attempt, []ItemResult{{ID: ids(0), Status: ItemSucceeded}}); !errors.Is(err, ErrAttempt) {
		t.Fatalf("a running attempt was recorded as stopped: %v", err)
	}
	if _, err := f.repo.commit(bg, f.job.ChildID, 1, running.Attempt, []ItemResult{{ID: ids(3), Status: ItemSucceeded}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.stop(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.commitAfterStop(bg, f.job.ChildID, 1, running.Attempt+1, []ItemResult{{ID: ids(0), Status: ItemSucceeded}}); !errors.Is(err, ErrAttempt) {
		t.Fatalf("another attempt's results were recorded: %v", err)
	}
	stopped, err := f.repo.commitAfterStop(bg, f.job.ChildID, 1, running.Attempt, []ItemResult{
		{ID: ids(0), Status: ItemSucceeded}, {ID: ids(1), Status: ItemFailed, Error: "cancelled"}, {ID: ids(2), Status: ItemSkipped}, {ID: ids(3), Status: ItemSkipped},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ItemStatus{ItemSucceeded, ItemPending, ItemSkipped, ItemSucceeded}
	for i, result := range stopped.Results {
		if result.Status != want[i] {
			t.Fatalf("object %d is %s, want %s", i, result.Status, want[i])
		}
	}
	if stopped.Status != Stopped || stopped.Attempt != running.Attempt+1 || stopped.Counters.Succeeded != 2 || stopped.Counters.Skipped != 1 || stopped.Counters.Failed != 0 {
		t.Fatalf("stopped record: %s attempt %d %+v", stopped.Status, stopped.Attempt, stopped.Counters)
	}
	// A newer fence, or a deletion, ends the window.
	if err := f.repo.fenceParent(bg, f.job.ParentID, 2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.commitAfterStop(bg, f.job.ChildID, 1, running.Attempt, []ItemResult{{ID: ids(1), Status: ItemSucceeded}}); !errors.Is(err, ErrGeneration) {
		t.Fatalf("results recorded after a newer fence: %v", err)
	}
	if err := f.repo.fenceParent(bg, f.job.ParentID, 2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.commitAfterStop(bg, f.job.ChildID, 2, running.Attempt, []ItemResult{{ID: ids(1), Status: ItemSucceeded}}); !errors.Is(err, ErrDeleted) && !errors.Is(err, ErrAttempt) {
		t.Fatalf("results recorded for a deleted task: %v", err)
	}
	record, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Results[1].Status != ItemPending {
		t.Fatalf("object 1 is %s after the window closed", record.Results[1].Status)
	}
}

// hookDB runs hook once, before the next update, and reports when that
// update has ended.
type hookDB struct {
	Database
	mu      sync.Mutex
	hook    func()
	updated chan struct{}
}

func (d *hookDB) Update(fn func(*state.Tx) error) error {
	d.mu.Lock()
	hook := d.hook
	d.hook = nil
	d.mu.Unlock()
	if hook == nil {
		return d.Database.Update(fn)
	}
	hook()
	defer close(d.updated)
	return d.Database.Update(fn)
}

// A stop can fence the attempt out between its last check and a periodic
// commit of results. The commit then fails, and the stop cancels the attempt
// right after: the results stay, and the attempt records them after the stop.
func TestStopBetweenCheckAndCommitKeepsTheBatch(t *testing.T) {
	f := newFixture(t, 3)
	db := &hookDB{Database: f.db, updated: make(chan struct{})}
	f.repo, _ = NewRepository(db, RepositoryOptions{})
	store := &heldStore{hold: map[string]bool{f.job.Items[1].DestinationKey: true, f.job.Items[2].DestinationKey: true}, held: make(chan struct{}, 2), first: make(chan struct{})}
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		store.Store = dst
		return src, store, err
	}), Options{WorkersPerJob: 4, CommitBatchSize: 1, CommitInterval: time.Hour})
	f.acceptStart()
	awaitSignals(t, store.held, 2, "the held writes")
	// The first result's commit finds the record already stopped.
	db.mu.Lock()
	db.hook = func() {
		if _, err := f.repo.stop(bg, f.job.ChildID, 1); err != nil {
			t.Error(err)
		}
	}
	db.mu.Unlock()
	close(store.first)
	select {
	case <-db.updated:
	case <-time.After(time.Minute): // hang guard only
		t.Fatal("the result was never committed")
	}
	if err := f.engine.Stop(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	record, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != Stopped || record.Results[0].Status != ItemSucceeded || record.Results[1].Status != ItemPending || record.Results[2].Status != ItemPending {
		t.Fatalf("stopped record %s: %+v", record.Status, record.Results)
	}
}

// heldStore writes other objects once first closes and holds the writes of
// the keys in hold until their context ends.
type heldStore struct {
	transferstore.Store
	hold  map[string]bool
	held  chan struct{}
	first chan struct{}
}

func (s *heldStore) Write(ctx context.Context, key string, reader io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if s.hold[key] {
		s.held <- struct{}{}
		<-ctx.Done()
		return transferstore.WriteResult{}, ctx.Err()
	}
	select {
	case <-s.first:
	case <-ctx.Done():
		return transferstore.WriteResult{}, ctx.Err()
	}
	return s.Store.Write(ctx, key, reader, opts)
}
