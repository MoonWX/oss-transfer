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

// A task that turned ranged downloads off streams every object, however
// large and however idle the node; an automatic task uses ranges.
func TestTaskRangedDownloadChoice(t *testing.T) {
	payload := downloadPayload(8*localTestPart + 5)
	lender := idlePartLender(100)
	source, counter, target, root, stat := downloadFixture(t, payload, 4, lender)
	for _, test := range []struct {
		choice              string
		gets, ranged, reads int64
	}{{RangedDownloadOff, 1, 0, 1}, {"", 10, 10, 9}} {
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
