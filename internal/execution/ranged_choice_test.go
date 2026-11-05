package execution

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// The choice is not part of a child's identity: a redelivery that adds it
// (as on resume after the Agent was upgraded) is the same work and updates
// the stored job, which the next attempt reads; unknown values are rejected.
func TestRedeliveryUpdatesTheRangedDownloadChoice(t *testing.T) {
	f := newFixture(t, 1)
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	next := f.job
	next.RangedDownload = RangedDownloadAuto
	record, err := f.repo.Accept(bg, next)
	if err != nil || record.Job.RangedDownload != RangedDownloadAuto {
		t.Fatalf("redelivery: %q (%v)", record.Job.RangedDownload, err)
	}
	if stored, err := f.repo.Get(bg, f.job.ChildID); err != nil || stored.Job.RangedDownload != RangedDownloadAuto {
		t.Fatalf("stored job keeps %q (%v)", stored.Job.RangedDownload, err)
	}
	bad := next
	bad.RangedDownload = "on"
	if _, err := f.repo.Accept(bg, bad); err == nil {
		t.Fatal("unknown ranged-download value accepted")
	}
}

// Only an explicit "auto" lets a LOCAL destination use ranges. A task that
// turned ranged downloads off, and a job without the field (accepted by an
// Agent that predated it, then recovered after an upgrade), stream every
// object however large and however idle the node.
func TestTaskRangedDownloadChoice(t *testing.T) {
	payload := downloadPayload(8*localTestPart + 5)
	lender := idlePartLender(100)
	source, counter, target, root, stat := downloadFixture(t, payload, 4, lender)
	for _, test := range []struct {
		choice              string
		gets, ranged, reads int64
	}{{RangedDownloadOff, 1, 0, 1}, {"", 1, 0, 1}, {RangedDownloadAuto, 10, 10, 9}} {
		var decorations atomic.Int64
		engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
			decorations.Add(1)
			return input, nil
		}}}
		counter.gets.Store(0)
		counter.ranged.Store(0)
		job := s3ToLocalJob(1)
		job.RangedDownload = test.choice
		key := "choice-" + test.choice + ".bin"
		if result := engine.copyItem(bg, job, Item{ID: key, Source: stat, DestinationKey: key}, source, target); result.Status != ItemSucceeded {
			t.Fatalf("copy failed: %+v", result)
		}
		if got, _ := os.ReadFile(filepath.Join(root, key)); !bytes.Equal(got, payload) {
			t.Fatal("target differs from source")
		}
		if counter.gets.Load() != test.gets || counter.ranged.Load() != test.ranged || decorations.Load() != test.reads {
			t.Fatalf("choice %q: %d GETs, %d ranged, %d reads; want %d, %d, %d", test.choice, counter.gets.Load(), counter.ranged.Load(), decorations.Load(), test.gets, test.ranged, test.reads)
		}
		if lender.Lent() != 0 {
			t.Fatal("borrowed slots not returned")
		}
	}
}
