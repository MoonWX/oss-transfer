package execution

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Objects an attempt published before a stop must be credited to it, not
// found at the destination by the resumed attempt and counted as skipped.
// Results are only committed per batch, so a stop arriving between batches
// used to lose them (put=129 skipped=2 in the CLI acceptance run).
func TestStopAfterPublishCreditsPublishedObjects(t *testing.T) {
	for _, stopParent := range []bool{true, false} {
		name := "stop-child"
		if stopParent {
			name = "stop-parent"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 3)
			f.job.ConflictPolicy = "SKIP"
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			// No periodic or size-triggered commit: everything rides on the
			// final persistence of the interrupted attempt.
			f.startEngine(nil, Options{WorkersPerJob: 3, CommitInterval: time.Hour, CommitBatchSize: 128, DecorateReader: func(ctx context.Context, _ Job, item Item, input io.ReadCloser) (io.ReadCloser, error) {
				if item.ID == "3" && blocked.CompareAndSwap(false, true) {
					return &blockingReader{ReadCloser: input, entered: entered, release: release, ctx: ctx}, nil
				}
				return input, nil
			}})
			f.acceptStart()
			<-entered
			// Wait until both quick objects are published.
			for _, item := range f.job.Items[:2] {
				for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Millisecond) {
					if _, err := os.Stat(filepath.Join(f.dst, item.DestinationKey)); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s was never published", item.ID)
					}
				}
			}
			var err error
			if stopParent {
				err = f.engine.StopParent(bg, f.job.ParentID, 1)
			} else {
				err = f.engine.Stop(bg, f.job.ChildID, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			stopped, err := f.repo.Get(bg, f.job.ChildID)
			if err != nil || stopped.Status != Stopped || stopped.Counters.Succeeded != 2 {
				t.Fatalf("stop lost published results: %+v %v", stopped.Counters, err)
			}
			close(release)
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
				t.Fatalf("resumed attempt counted its own objects as skipped: %+v", done.Counters)
			}
			f.assertFiles()
		})
	}
}
