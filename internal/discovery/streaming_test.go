package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// streamingSource holds one directory long enough to be examined while it
// is still being read (more than 4,096 names), between two short ones.
func streamingSource(t *testing.T) (string, int) {
	t.Helper()
	root := t.TempDir()
	total := 0
	for _, part := range []struct {
		name  string
		count int
	}{{"a-first", 3}, {"big", 4200}, {"z-last", 3}} {
		directory := filepath.Join(root, part.name)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < part.count; index++ {
			if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%05d.bin", index)), []byte("test data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		total += part.count
	}
	return root, total
}

// Keys handed to the committer are done until their commit, so a repeated
// name is not emitted twice even before its batch is durable.
func TestScanDoneReportsKeysQueuedForCommit(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	queued := &queuedKeys{}
	queued.add("queued.bin")
	queued.add("queued.bin")
	answers, err := scanner.committedThisPass(attempt, queued)([]string{"queued.bin", "other.bin"})
	if err != nil || !answers[0] || answers[1] {
		t.Fatalf("answers %v, %v", answers, err)
	}
	batch := pendingBatch{objects: []transferstore.Object{{Key: "queued.bin"}}}
	queued.committed([]pendingBatch{batch})
	if !queued.has("queued.bin") {
		t.Fatal("key forgotten while a second copy was still queued")
	}
	queued.committed([]pendingBatch{batch})
	if queued.has("queued.bin") {
		t.Fatal("committed key still queued")
	}
}

// A scan stopped while a long directory streams resumes from its saved
// cursor without counting or emitting any file twice; its first page's
// checkpoint says that resuming needs the scan protocol.
func TestStreamedScanResumesWithExactCounters(t *testing.T) {
	root, total := streamingSource(t)
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &checkpointLocalFactory{reached: make(chan struct{})}
	scanner, db := newScanner(t, path, factory, Options{})
	ctx := context.Background()
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	var before State
	for {
		current, err := scanner.Status(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.Checkpoint != nil && current.Checkpoint.Cursor != "" {
			before = current
			break
		}
		time.Sleep(time.Millisecond) // The page's checkpoint is being committed.
	}
	first, found, err := scanner.Batch(ctx, 1, 1, 0)
	if err != nil || !found {
		t.Fatalf("first batch: %v", err)
	}
	if _, err := scanner.Stop(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resumedFactory := &checkpointLocalFactory{}
	resumed, _ := newScanner(t, path, resumedFactory, Options{})
	if _, err := resumed.Resume(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, resumed, Completed)
	if done.TotalObjects != int64(total) || done.SeenObjects != int64(total) || done.MissingObjects != 0 || done.Checkpoint.Pass != before.Checkpoint.Pass {
		t.Fatalf("resumed streamed scan counted %+v", done)
	}
	assertBatches(t, resumed, 2, total)
	_, keys := resumedFactory.observed()
	if len(keys) != total-int(before.TotalObjects) {
		t.Fatalf("resume emitted %d objects after %d of %d were committed", len(keys), before.TotalObjects, total)
	}
	if first.Checkpoint == nil {
		t.Fatal("first batch carries no checkpoint")
	}
	streamedSeen := first.Checkpoint.RequiresScanProtocol == 1
	for after := int64(1); !streamedSeen; after++ {
		batch, found, err := resumed.Batch(ctx, 1, 2, after)
		if err != nil || !found {
			t.Fatalf("no checkpoint named the streamed directory: %v", err)
		}
		streamedSeen = batch.Checkpoint != nil && batch.Checkpoint.RequiresScanProtocol == 1
	}
}

// Another Agent takes a streamed scan over from Master's checkpoint: the
// files Master assigned are done there, and the counters end exact.
func TestStreamedScanTakeoverKeepsExactCounters(t *testing.T) {
	root, total := streamingSource(t)
	ctx := context.Background()
	old, _ := newScanner(t, filepath.Join(t.TempDir(), "old.db"), &checkpointLocalFactory{}, Options{})
	if _, err := old.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, old, Completed)
	var assigned []Batch
	for after := int64(0); ; after++ {
		batch, found, err := old.Batch(ctx, 1, 1, after)
		if err != nil || !found {
			t.Fatalf("no streamed checkpoint among the batches: %v", err)
		}
		assigned = append(assigned, batch)
		if batch.Checkpoint != nil && batch.Checkpoint.RequiresScanProtocol == 1 && !batch.Checkpoint.SourceDone && len(assigned) > 2 {
			break
		}
	}
	checkpoint := assigned[len(assigned)-1].Checkpoint
	factory := &checkpointLocalFactory{}
	fresh, _ := newScanner(t, filepath.Join(t.TempDir(), "new.db"), factory, Options{})
	request := takeoverRequest(localSpec(root), assigned...)
	request.Checkpoint = checkpoint
	seedTakeover(t, fresh, request)
	if _, err := fresh.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, fresh, Completed)
	if done.TotalObjects != int64(total) || done.SeenObjects != int64(total) || done.MissingObjects != 0 {
		t.Fatalf("takeover of a streamed scan counted %+v", done)
	}
	if keys := takeoverNewKeys(t, fresh, request); len(keys) != total-int(request.Objects) {
		t.Fatalf("takeover added %d objects, want %d", len(keys), total-int(request.Objects))
	}
}

// A key that leaves the queue while Done reads the index was committed
// before it left: Done reports it either way.
func TestScanDoneKeepsAKeyCommittedWhileItAsks(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	ctx := context.Background()
	queued := &queuedKeys{}
	done := scanner.committedThisPass(attempt, queued)
	for i := range 200 {
		key := fmt.Sprintf("key-%03d", i)
		queued.add(key)
		batch := pendingBatch{objects: []transferstore.Object{{Key: key, Size: 1}}}
		committed := make(chan error, 1)
		go func() {
			err := scanner.commitBatches(ctx, attempt, []pendingBatch{batch})
			if err == nil {
				queued.committed([]pendingBatch{batch})
			}
			committed <- err
		}()
		answers, err := done([]string{key})
		if err != nil || !answers[0] {
			t.Fatalf("%s: answers %v, %v", key, answers, err)
		}
		if err := <-committed; err != nil {
			t.Fatal(err)
		}
	}
}
