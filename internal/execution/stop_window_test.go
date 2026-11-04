package execution

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func startPausedBarrier(t *testing.T, f *fixture, opts Options) (entered, release chan struct{}) {
	t.Helper()
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		source, destination, err := base.Open(ctx, j)
		if err != nil {
			return nil, nil, err
		}
		return source, &pausingSync{Local: destination.(*transferstore.Local), once: &once, entered: entered, release: release}, nil
	}), opts)
	return entered, release
}

func waitPublished(t *testing.T, f *fixture) {
	t.Helper()
	for _, item := range f.job.Items {
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Millisecond) {
			if _, err := os.Stat(filepath.Join(f.dst, item.DestinationKey)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never published", item.ID)
			}
		}
	}
}

func resumeAndExpectAllSucceeded(t *testing.T, f *fixture, generation uint64) {
	t.Helper()
	if generation != f.job.Generation {
		f.job.Generation = generation
		if _, err := f.engine.Accept(bg, f.job); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.engine.Resume(bg, f.job.ChildID, generation, false); err != nil {
		t.Fatal(err)
	}
	done := f.wait()
	if done.Status != Completed || done.Counters.Succeeded != int64(len(f.job.Items)) || done.Counters.Skipped != 0 {
		t.Fatalf("resumed attempt recounted its own objects: %+v", done.Counters)
	}
}

// F1: no flush ever runs; the stop fence lands while the final batch is in
// its barrier, so finish is rejected. The batch must still be credited.
func TestStopFenceDuringFinalBarrierCreditsBatch(t *testing.T) {
	for _, stopParent := range []bool{false, true} {
		name := "stop-child"
		if stopParent {
			name = "stop-parent"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 3)
			f.job.ConflictPolicy = "SKIP"
			entered, release := startPausedBarrier(t, f, Options{WorkersPerJob: 3, CommitInterval: time.Hour, CommitBatchSize: 128})
			f.acceptStart()
			<-entered // Every result is in; the final barrier is paused.
			waitPublished(t, f)
			// The stop: fence first, release the barrier (finish now meets the
			// fence), then the stop's cancel and wait.
			var err error
			if stopParent {
				err = f.repo.fenceParent(bg, f.job.ParentID, 1, false)
			} else {
				_, err = f.repo.stop(bg, f.job.ChildID, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			if stopParent {
				err = f.engine.StopParent(bg, f.job.ParentID, 1)
			} else {
				err = f.engine.Stop(bg, f.job.ChildID, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			stopped, err := f.repo.Get(bg, f.job.ChildID)
			if err != nil || stopped.Status != Stopped || stopped.Counters.Succeeded != 3 {
				t.Fatalf("final batch was dropped by the stop: %s %+v %v", stopped.Status, stopped.Counters, err)
			}
			generation := uint64(1)
			if stopParent {
				generation = 2
				if err := f.engine.ResumeParent(bg, f.job.ParentID, generation, false); err != nil {
					t.Fatal(err)
				}
			}
			resumeAndExpectAllSucceeded(t, f, generation)
		})
	}
}

// F2: ResumeParent's fence-only stop marks the parent first and each child
// later, in separate transactions, before it cancels. A flush rejected in
// that window must wait for the child's stop instead of crediting early.
func TestFenceOnlyParentStopCreditsRejectedFlush(t *testing.T) {
	f := newFixture(t, 3)
	f.job.ConflictPolicy = "SKIP"
	entered, release := startPausedBarrier(t, f, Options{WorkersPerJob: 3, CommitInterval: time.Hour, CommitBatchSize: 1})
	crediting := make(chan struct{})
	var creditOnce sync.Once
	f.engine.creditWaiting = func() { creditOnce.Do(func() { close(crediting) }) }
	f.acceptStart()
	<-entered // The first flush waits in its barrier.
	waitPublished(t, f)
	// Phase one of ResumeParent's fence: the parent only; the child is still
	// Running at the same attempt.
	if err := f.repo.fenceParent(bg, f.job.ParentID, 1, false, true); err != nil {
		t.Fatal(err)
	}
	close(release)
	// The flush is rejected (parent stopped) and the attempt drains all its
	// results while the child is not yet stopped. Only then does the rest of
	// ResumeParent run: stop the child, cancel, wait, advance the generation.
	<-crediting
	if err := f.engine.ResumeParent(bg, f.job.ParentID, 2, false); err != nil {
		t.Fatal(err)
	}
	record, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || record.Counters.Succeeded != 3 {
		t.Fatalf("rejected flush dropped results before the child stop: %s %+v %v", record.Status, record.Counters, err)
	}
	resumeAndExpectAllSucceeded(t, f, 2)
}
