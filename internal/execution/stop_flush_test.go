package execution

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// pausingSync blocks the first barrier until released, so a test can land a
// stop fence between a flush's barrier and its commit.
type pausingSync struct {
	*transferstore.Local
	once             *sync.Once
	entered, release chan struct{}
}

func (p *pausingSync) Sync(ctx context.Context) error {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return p.Local.Sync(ctx)
}

// Stop commits its fence before it cancels the attempt. A flush whose commit
// lands in between is rejected while its context is still live; its results
// must still be credited to the stopped attempt instead of being dropped and
// recounted as skipped after the resume.
func TestFlushRejectedByStopFenceKeepsResults(t *testing.T) {
	for _, stopParent := range []bool{false, true} {
		name := "stop-child"
		if stopParent {
			name = "stop-parent"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 3)
			f.job.ConflictPolicy = "SKIP"
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			base := f.factory()
			f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
				source, destination, err := base.Open(ctx, j)
				if err != nil {
					return nil, nil, err
				}
				return source, &pausingSync{Local: destination.(*transferstore.Local), once: &once, entered: entered, release: release}, nil
			}), Options{WorkersPerJob: 3, CommitInterval: time.Hour, CommitBatchSize: 1})
			f.acceptStart()
			// The first result triggers a flush, which now waits in its barrier.
			<-entered
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
			// The stop's fence commits; its cancel has not happened yet.
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
			stopped := f.wait()
			if stopped.Status != Stopped || stopped.Counters.Succeeded != 3 {
				t.Fatalf("fenced flush dropped published results: %s %+v", stopped.Status, stopped.Counters)
			}
			generation := uint64(1)
			if stopParent {
				generation = 2
				if err := f.engine.ResumeParent(bg, f.job.ParentID, generation, false); err != nil {
					t.Fatal(err)
				}
				f.job.Generation = generation
				if _, err := f.engine.Accept(bg, f.job); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.engine.Resume(bg, f.job.ChildID, generation, false); err != nil {
				t.Fatal(err)
			}
			done := f.wait()
			if done.Status != Completed || done.Counters.Succeeded != 3 || done.Counters.Skipped != 0 {
				t.Fatalf("resumed attempt recounted its own objects: %+v", done.Counters)
			}
		})
	}
}

// If run unwinds by panic (a group commit re-panics in its leader), workers
// blocked on reporting a result must still exit and release their slots.
func TestPanickingCommitReleasesBlockedWorkers(t *testing.T) {
	f := newFixture(t, 6)
	var panicked atomic.Bool
	f.repo.commits.batching = func([]*commitRequest) {
		if panicked.CompareAndSwap(false, true) {
			panic("injected commit panic")
		}
	}
	f.startEngine(nil, Options{WorkersPerJob: 6, QueueSize: 1, CommitInterval: time.Hour, CommitBatchSize: 1})
	f.acceptStart()
	record := f.wait()
	if record.Status == Completed || !panicked.Load() {
		t.Fatalf("the injected panic did not end the attempt: %s", record.Status)
	}
	for deadline := time.Now().Add(time.Minute); f.engine.activeObjects.Load() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("workers leaked: %d object slots still held", f.engine.activeObjects.Load())
		}
	}
}
