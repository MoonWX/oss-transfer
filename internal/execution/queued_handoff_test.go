package execution

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type handoffCountedStore struct {
	transferstore.Store
	writes *atomic.Int64
}

func newHandoffFixture(t *testing.T, count int) *fixture {
	t.Helper()
	f := newFixture(t, count)
	if err := f.db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (s handoffCountedStore) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.writes.Add(1)
	return s.Store.Write(ctx, key, input, options)
}

func handoffRequest(job Job) QueueReleaseRequest {
	return QueueReleaseRequest{ParentID: job.ParentID, ChildID: job.ChildID, Generation: job.Generation, Assignment: job.Assignment, OwnerEpoch: job.OwnerEpoch}
}

func handoffFactory(base StoreFactory, writes *atomic.Int64, entered chan<- struct{}, gate <-chan struct{}) StoreFactory {
	return StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		source, destination, err := base.Open(ctx, job)
		if err != nil {
			return nil, nil, err
		}
		if entered != nil {
			entered <- struct{}{}
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				_ = source.Close()
				_ = destination.Close()
				return nil, nil, ctx.Err()
			}
		}
		return source, handoffCountedStore{destination, writes}, nil
	})
}

func reopenHandoffFixture(t *testing.T, f *fixture, factory StoreFactory) {
	t.Helper()
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.engine = nil
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.startEngine(factory, Options{})
}

func assertHandoffOldOwnerFenced(t *testing.T, f *fixture, old Job) {
	t.Helper()
	if _, err := f.engine.Accept(bg, old); !errors.Is(err, ErrAssignment) {
		t.Fatalf("old child_accept bypassed release: %v", err)
	}
	if err := f.engine.TryStart(bg, old.ChildID, old.Generation); !errors.Is(err, ErrAssignment) {
		t.Fatalf("old child_start bypassed release: %v", err)
	}
	if err := f.engine.Resume(bg, old.ChildID, old.Generation, false); !errors.Is(err, ErrAssignment) {
		t.Fatalf("old child_resume bypassed release: %v", err)
	}
	pending, err := f.repo.Pending(bg, "", 16)
	if err != nil || len(pending) != 0 {
		t.Fatalf("released job remained in ready queue: %+v %v", pending, err)
	}
}

func TestQueuedHandoffReleaseSurvivesLostResponseAndAgentRestart(t *testing.T) {
	f := newHandoffFixture(t, 2)
	writes := &atomic.Int64{}
	factory := handoffFactory(f.factory(), writes, nil, nil)
	f.startEngine(factory, Options{})
	seed, err := f.engine.Accept(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	old := f.job
	request := handoffRequest(old)
	if released, err := f.engine.ReleaseQueued(bg, request); err != nil || !released {
		t.Fatalf("release waiting job: %v %v", released, err)
	}
	// Master never received the first ACK. Retrying the same command must
	// recover its outcome even after both the process and database reopen.
	reopenHandoffFixture(t, f, factory)
	if released, err := f.engine.ReleaseQueued(bg, request); err != nil || !released {
		t.Fatalf("lost release ACK became ambiguous after restart: %v %v", released, err)
	}
	assertHandoffOldOwnerFenced(t, f, old)
	if writes.Load() != 0 {
		t.Fatalf("released old owner wrote %d objects", writes.Load())
	}

	// A later assignment can legitimately return to this Agent. Its seed is
	// explicit; the release fence must never authorize the old assignment.
	f.job.Assignment++
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrAssignment) {
		t.Fatalf("new assignment replaced released work without seed: %v", err)
	}
	if _, err := f.engine.AcceptSeed(bg, f.job, &seed); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, f.job.Generation); err != nil {
		t.Fatal(err)
	}
	result := f.wait()
	if result.Status != Completed || result.Counters.Succeeded != 2 || writes.Load() != 2 {
		t.Fatalf("new owner did not execute exactly once: %+v writes=%d", result, writes.Load())
	}
	if _, err := f.engine.Accept(bg, old); !errors.Is(err, ErrAssignment) {
		t.Fatalf("late old admission reset the new assignment: %v", err)
	}
	if released, err := f.engine.ReleaseQueued(bg, request); released || !errors.Is(err, ErrAssignment) {
		t.Fatalf("old release revoked a newer assignment: %v %v", released, err)
	}
	f.assertFiles()
}

func TestQueuedHandoffStartReleaseRaceNeverRunsBothOwners(t *testing.T) {
	// Explicit release-first and start-first checks below cover both orderings;
	// simultaneous starts also exercise mutex/admission ordering under -race.
	for iteration := range 12 {
		t.Run(string(rune('a'+iteration)), func(t *testing.T) {
			old := newHandoffFixture(t, 2)
			writes := &atomic.Int64{}
			gate := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(gate) }) }
			t.Cleanup(unblock)
			old.startEngine(handoffFactory(old.factory(), writes, nil, gate), Options{})
			seed, err := old.engine.Accept(bg, old.job)
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			started := make(chan error, 1)
			type releaseResult struct {
				released bool
				err      error
			}
			released := make(chan releaseResult, 1)
			go func() { <-ready; started <- old.engine.TryStart(bg, old.job.ChildID, old.job.Generation) }()
			go func() {
				<-ready
				yes, err := old.engine.ReleaseQueued(bg, handoffRequest(old.job))
				released <- releaseResult{yes, err}
			}()
			close(ready)
			startErr, release := <-started, <-released
			if release.err != nil {
				t.Fatal(release.err)
			}
			unblock()
			if release.released {
				if !errors.Is(startErr, ErrAssignment) {
					t.Fatalf("release succeeded while old owner was admitted: %v", startErr)
				}
				assertHandoffOldOwnerFenced(t, old, old.job)
				if writes.Load() != 0 {
					t.Fatal("old owner wrote after release")
				}
				fresh := newHandoffFixture(t, 2)
				fresh.job = old.job
				fresh.job.Assignment++
				fresh.startEngine(handoffFactory(fresh.factory(), writes, nil, nil), Options{})
				if _, err := fresh.engine.AcceptSeed(bg, fresh.job, &seed); err != nil {
					t.Fatal(err)
				}
				if err := fresh.engine.TryRecover(bg, fresh.job.ChildID, fresh.job.Generation); err != nil {
					t.Fatal(err)
				}
				if result := fresh.wait(); result.Status != Completed || result.Counters.Succeeded != 2 {
					t.Fatal(result)
				}
			} else {
				if startErr != nil {
					t.Fatalf("neither owner retained the queued work: %v", startErr)
				}
				if result := old.wait(); result.Status != Completed || result.Counters.Succeeded != 2 {
					t.Fatal(result)
				}
			}
			if writes.Load() != 2 {
				t.Fatalf("handoff lost or duplicated writes: %d", writes.Load())
			}
			old.assertFiles()
		})
	}
}

func TestQueuedHandoffRefusesRunningAndRecoverableWork(t *testing.T) {
	f := newHandoffFixture(t, 2)
	writes := &atomic.Int64{}
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	f.startEngine(handoffFactory(f.factory(), writes, entered, gate), Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.TryStart(bg, f.job.ChildID, f.job.Generation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("running attempt never entered storage factory")
	}
	if released, err := f.engine.ReleaseQueued(bg, handoffRequest(f.job)); err != nil || released {
		t.Fatalf("running work was moved: %v %v", released, err)
	}
	// Cancel the held I/O normally, leaving a recoverable attempt on disk.
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.engine = nil
	close(gate)
	f.startEngine(handoffFactory(f.factory(), writes, nil, nil), Options{})
	record, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || record.Status != Recoverable || record.Attempt == 0 {
		t.Fatalf("expected a real interrupted attempt: %+v %v", record, err)
	}
	if released, err := f.engine.ReleaseQueued(bg, handoffRequest(f.job)); err != nil || released {
		t.Fatalf("previously running work was moved as unstarted: %v %v", released, err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, f.job.Generation); err != nil {
		t.Fatal(err)
	}
	if result := f.wait(); result.Status != Completed || result.Counters.Succeeded != 2 || writes.Load() != 2 {
		t.Fatalf("refusal disturbed recovery: %+v writes=%d", result, writes.Load())
	}
	f.assertFiles()
}

func TestQueuedHandoffUnknownJobDoesNotAcknowledgeRelease(t *testing.T) {
	f := newHandoffFixture(t, 1)
	f.startEngine(nil, Options{})
	if released, err := f.engine.ReleaseQueued(bg, handoffRequest(f.job)); err != nil || released {
		t.Fatalf("missing delivery was falsely acknowledged as released: %v %v", released, err)
	}
	// No handoff was authorized; the original in-flight acceptance remains valid.
	f.acceptStart()
	if result := f.wait(); result.Status != Completed {
		t.Fatal(result)
	}
	f.assertFiles()
}

func TestQueuedHandoffRequiresExactOwnershipForReleaseAndReplay(t *testing.T) {
	f := newHandoffFixture(t, 1)
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	request := handoffRequest(f.job)
	variants := []struct {
		name string
		edit func(*QueueReleaseRequest)
	}{
		{"parent", func(r *QueueReleaseRequest) { r.ParentID++ }},
		{"assignment", func(r *QueueReleaseRequest) { r.Assignment++ }},
		{"owner_epoch", func(r *QueueReleaseRequest) { r.OwnerEpoch++ }},
		{"newer_generation", func(r *QueueReleaseRequest) { r.Generation++ }},
		{"older_generation", func(r *QueueReleaseRequest) { r.Generation-- }},
	}
	for _, phase := range []string{"waiting", "released"} {
		for _, variant := range variants {
			t.Run(phase+"/"+variant.name, func(t *testing.T) {
				invalid := request
				variant.edit(&invalid)
				if released, err := f.engine.ReleaseQueued(bg, invalid); released {
					t.Fatalf("mismatched ownership acknowledged release: %v", err)
				}
			})
		}
		row, err := f.repo.Get(bg, f.job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		if phase == "waiting" && (row.Status != Waiting || row.ReleasedAssignment != nil || row.Attempt != 0) {
			t.Fatalf("invalid requests mutated unstarted work: %+v", row)
		}
		if released, err := f.engine.ReleaseQueued(bg, request); err != nil || !released {
			t.Fatalf("exact ownership failed in %s phase: %v %v", phase, released, err)
		}
	}
	assertHandoffOldOwnerFenced(t, f, f.job)
}

func TestQueuedHandoffCanMoveNeverStartedFailoverSeed(t *testing.T) {
	first := newHandoffFixture(t, 2)
	first.startEngine(nil, Options{})
	seed, err := first.engine.Accept(bg, first.job)
	if err != nil {
		t.Fatal(err)
	}
	// The first owner disappears without ever starting. The surviving Agent
	// durably imports the queue, but has not started this child when its peer
	// returns. This is still unstarted work, not an interrupted transfer.
	second := newHandoffFixture(t, 2)
	second.job = first.job
	second.job.Assignment++
	writes := &atomic.Int64{}
	second.startEngine(handoffFactory(second.factory(), writes, nil, nil), Options{})
	imported, err := second.engine.AcceptSeed(bg, second.job, &seed)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Attempt != 0 || imported.StartedAt != nil {
		t.Fatalf("importing an unstarted queue fabricated an execution: %+v", imported)
	}
	if released, err := second.engine.ReleaseQueued(bg, handoffRequest(second.job)); err != nil || !released {
		t.Fatalf("never-started failover queue cannot be rebalanced: %v %v", released, err)
	}
	assertHandoffOldOwnerFenced(t, second, second.job)
	if _, err := second.engine.AcceptSeed(bg, second.job, &seed); !errors.Is(err, ErrAssignment) {
		t.Fatalf("late original failover seed revived released work: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("unstarted owner performed writes")
	}
	third := newHandoffFixture(t, 2)
	third.job = second.job
	third.job.Assignment++
	third.startEngine(handoffFactory(third.factory(), writes, nil, nil), Options{})
	if _, err := third.engine.AcceptSeed(bg, third.job, &imported); err != nil {
		t.Fatal(err)
	}
	if err := third.engine.TryRecover(bg, third.job.ChildID, third.job.Generation); err != nil {
		t.Fatal(err)
	}
	if row := third.wait(); row.Status != Completed || row.Counters.Succeeded != 2 || writes.Load() != 2 {
		t.Fatalf("two handoffs lost or duplicated work: %+v writes=%d", row, writes.Load())
	}
	first.assertFiles()
}
