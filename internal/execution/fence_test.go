package execution

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestHigherGenerationStopCancelsAndPersistsWithoutResuming(t *testing.T) {
	f := newFixture(t, 2)
	ready := make(chan struct{})
	var closed atomic.Int64
	f.startEngine(nil, Options{WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		close(ready)
		return &cancelReader{input: input, ctx: ctx, closed: &closed}, nil
	}})
	f.acceptStart()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("source never opened")
	}
	before, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.StopParent(bg, f.job.ParentID, 3); err != nil {
		t.Fatalf("new generation stop failed on lagging Agent: %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("stop returned with live source: %d", closed.Load())
	}
	stopped, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || stopped.Status != Stopped || stopped.Job.Generation != 3 || stopped.Counters.Succeeded != 0 {
		t.Fatalf("stop did not persist new generation: %+v %v", stopped, err)
	}
	if _, err := f.repo.commit(bg, f.job.ChildID, 1, before.Attempt, []ItemResult{{ID: "1", Status: ItemSucceeded}}); !errors.Is(err, ErrGeneration) {
		t.Fatalf("old completion bypassed new fence: %v", err)
	}
	if err := f.engine.StopParent(bg, f.job.ParentID, 2); !errors.Is(err, ErrGeneration) {
		t.Fatalf("older stop accepted: %v", err)
	}
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrGeneration) {
		t.Fatalf("old Accept bypassed new fence: %v", err)
	}
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.engine = nil
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	f.job.Generation = 3
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrStopped) {
		t.Fatalf("restart lost stop fence: %v", err)
	}
	if err := f.engine.ResumeParent(bg, f.job.ParentID, 4, true); err != nil {
		t.Fatal(err)
	}
	f.job.Generation = 4
	f.acceptStart()
	if result := f.wait(); result.Status != Completed || result.Counters.Succeeded != 2 {
		t.Fatalf("subsequent explicit resume failed: %+v", result)
	}
	f.assertFiles()
}

func TestFutureStopBeforeFirstChildCreatesDurableFence(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	if err := f.engine.StopParent(bg, f.job.ParentID, 5); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{1, 4, 5} {
		f.job.Generation = generation
		_, err := f.engine.Accept(bg, f.job)
		want := ErrGeneration
		if generation == 5 {
			want = ErrStopped
		}
		if !errors.Is(err, want) {
			t.Fatalf("generation %d bypassed empty fence: %v", generation, err)
		}
	}
	if parent, err := f.repo.Parent(bg, f.job.ParentID); err != nil || parent.Generation != 5 || !parent.Stopped {
		t.Fatalf("empty fence not durable: %+v %v", parent, err)
	}
}
