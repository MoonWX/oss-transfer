package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func waitBatchesReleased(t *testing.T, scanner *Scanner, parentID, through int64) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := scanner.Status(context.Background(), parentID)
		if err != nil {
			t.Fatal(err)
		}
		if current.ReleasedThrough == through {
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("acknowledged batches were not released")
	return State{}
}

func TestBatchReleaseResumesBoundedCleanupAndRetainsUnacknowledgedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	const total = cleanupPageKeys + 7
	const acknowledged = total - 2
	if err := db.Update(func(tx *state.Tx) error {
		current := State{Spec: localSpec(t.TempDir()), Generation: 1, Status: Stopped, TotalObjects: total, LastBatch: total, AckedThrough: acknowledged}
		if err := tx.Put(states, parentKey(1), current); err != nil {
			return err
		}
		// This begins as an old acknowledged scan without release permission.
		for n := int64(1); n <= total; n++ {
			if err := tx.Put(batches, batchKey(1, n), Batch{ParentID: 1, Generation: 1, Number: n, Objects: []transferstore.Object{{Key: "source", Size: 1}}, Bytes: 1}); err != nil {
				return err
			}
		}
		if err := tx.Put(objects, objectKey(1, "source"), discoveredObject{Object: transferstore.Object{Key: "source", Size: 1}}); err != nil {
			return err
		}
		return tx.Put(batches, batchKey(2, 1), Batch{ParentID: 2, Number: 1})
	}); err != nil {
		t.Fatal(err)
	}
	manual := &Scanner{store: db, active: make(map[int64]*running)}
	if _, err := manual.Release(context.Background(), 1, 1, acknowledged); err != nil {
		t.Fatal(err)
	}
	if done, err := manual.cleanupPage(context.Background(), 1); err != nil || done {
		t.Fatalf("cleanup was not bounded: done=%v err=%v", done, err)
	}
	if n := countCleanupRows(t, db, batches, 1); n != total-cleanupPageKeys {
		t.Fatalf("first cleanup removed %d rows", total-n)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, db := newScanner(t, path, &localFactory{}, Options{})
	current := waitBatchesReleased(t, scanner, 1, acknowledged)
	if current.AckedThrough != acknowledged || current.LastBatch != total || current.TotalObjects != total {
		t.Fatalf("release changed scanner accounting: %+v", current)
	}
	if n := countCleanupRows(t, db, objects, 1); n != 1 {
		t.Fatal("incomplete scan lost its recovery index")
	}
	if n := countCleanupRows(t, db, batches, 2); n != 1 {
		t.Fatal("cleanup crossed parent boundary")
	}
	for _, n := range []int64{0, acknowledged - 1} {
		if _, _, err := scanner.Batch(context.Background(), 1, 1, n); !errors.Is(err, ErrBatchReleased) {
			t.Fatalf("released cursor %d: %v", n, err)
		}
	}
	for after := int64(acknowledged); after < total; after++ {
		batch, found, err := scanner.Batch(context.Background(), 1, 1, after)
		if err != nil || !found || batch.Number != after+1 || len(batch.Objects) != 1 {
			t.Fatalf("unacknowledged tail lost: after=%d found=%v err=%v", after, found, err)
		}
	}
	if _, err := scanner.Ack(context.Background(), 1, 1, total+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future ACK accepted: %v", err)
	}
	if _, err := scanner.Ack(context.Background(), 1, 1, acknowledged); err != nil {
		t.Fatalf("duplicate ACK failed: %v", err)
	}
}

func TestBatchReleaseRollsBackWhenReaderFenceCannotBeRecorded(t *testing.T) {
	scanner, db, attempt := commitFixture(t)
	if err := scanner.commitBatches(context.Background(), attempt, []pendingBatch{scanTestBatch(0)}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		current, err := get(tx, 1)
		if err != nil {
			return err
		}
		current.AckedThrough, current.ReleasableThrough = 1, 1
		if err := tx.Put(states, parentKey(1), current); err != nil {
			return err
		}
		return tx.Put("metadata", "format", map[string]any{"role": "master", "schemaVersion": state.SchemaCompressedValues})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.cleanupPage(context.Background(), 1); err == nil {
		t.Fatal("cleanup bypassed reader fence")
	}
	current, err := scanner.Status(context.Background(), 1)
	if err != nil || current.ReleasedThrough != 0 {
		t.Fatalf("failed cleanup advanced watermark: %+v %v", current, err)
	}
	assertBatches(t, scanner, 1, 128)
}

func TestTakeoverBatchReleaseSkipsSeedPrefixAndKeepsReplayAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 3)
	request := takeoverRequest(localSpec(root), takeoverBatch(1, takeoverObject(t, root, "0000.txt")))
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &localFactory{}
	scanner, db := newScanner(t, path, factory, Options{MaxBatchObjects: 1})
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, scanner, Completed)
	if _, err := scanner.Release(ctx, 1, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 1) // Seeded batch 1 never existed locally.
	if n := countCleanupRows(t, db, batches, 1); n != 2 {
		t.Fatalf("unacknowledged takeover batches changed: %d", n)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, db = newScanner(t, path, factory, Options{MaxBatchObjects: 1})
	if _, err := scanner.Ack(ctx, 1, 1, 3, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(ctx, 1, 1, 3, 1); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 3)
	if n := countCleanupRows(t, db, batches, 1); n != 0 {
		t.Fatalf("acknowledged takeover batches retained: %d", n)
	}
	if _, _, err := scanner.Batch(ctx, 1, 1, 1, 1); !errors.Is(err, ErrBatchReleased) {
		t.Fatalf("released takeover cursor: %v", err)
	}
	if _, err := scanner.SeedTakeover(ctx, request); err != nil {
		t.Fatalf("release broke seed replay: %v", err)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil || factory.scans.Load() != 1 {
		t.Fatalf("release broke finalize replay: %v scans=%d", err, factory.scans.Load())
	}
}

func TestBatchCleanupKeepsLifecycleLockUntilWatermarkCommit(t *testing.T) {
	scanner, db, attempt := commitFixture(t)
	scanner.cleanupCancel()
	<-scanner.cleanupDone
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	if err := scanner.commitBatches(context.Background(), attempt, []pendingBatch{scanTestBatch(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Ack(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	writerReady, releaseWriter, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		writerDone <- db.Update(func(*state.Tx) error {
			close(writerReady)
			<-releaseWriter
			return nil
		})
	}()
	<-writerReady
	cleanupDone := make(chan error, 1)
	go func() { _, err := scanner.cleanupPage(context.Background(), 1); cleanupDone <- err }()
	// The held database writer puts cleanup exactly between its lifecycle lock
	// and its deletion transaction, without relying on source timing.
	deadline := time.Now().Add(time.Second)
	locked := false
	for time.Now().Before(deadline) {
		if !scanner.mu.TryLock() {
			locked = true
			break
		}
		scanner.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if !locked {
		close(releaseWriter)
		<-writerDone
		<-cleanupDone
		t.Fatal("cleanup released the lifecycle lock before its durable watermark")
	}
	// Start, Resume and takeover take their State snapshot under this lock.
	// Their snapshot must see the release, never the pre-deletion watermark.
	lifecycleRead := make(chan State, 1)
	go func() {
		scanner.mu.Lock()
		current, _ := scanner.Status(context.Background(), 1)
		scanner.mu.Unlock()
		lifecycleRead <- current
	}()
	select {
	case current := <-lifecycleRead:
		close(releaseWriter)
		<-writerDone
		<-cleanupDone
		t.Fatalf("lifecycle snapshot crossed blocked cleanup: %+v", current)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	current := <-lifecycleRead
	if current.ReleasedThrough != 1 || current.AckedThrough != 1 {
		t.Fatalf("lifecycle saw a stale watermark: %+v", current)
	}
}

func TestAckCannotBeOverwrittenByAnAttemptPublishedFromOlderState(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	scanner.cleanupCancel()
	<-scanner.cleanupDone
	if err := scanner.commitBatches(context.Background(), attempt, []pendingBatch{scanTestBatch(0)}); err != nil {
		t.Fatal(err)
	}
	scanner.finish(attempt, context.Canceled)
	snapshotRead, publishAttempt := make(chan struct{}), make(chan struct{})
	calls := 0
	scanner.options.ParentContext = func(int64, uint64) (context.Context, error) {
		calls++
		if calls == 2 { // launchLocked has read State, but has not saved it yet.
			close(snapshotRead)
			<-publishAttempt
		}
		return context.Background(), nil
	}
	startDone := make(chan error, 1)
	go func() { _, err := scanner.Start(context.Background(), attempt.Spec); startDone <- err }()
	<-snapshotRead
	ackDone := make(chan error, 1)
	go func() { _, err := scanner.Ack(context.Background(), 1, 1, 1); ackDone <- err }()
	select {
	case err := <-ackDone:
		close(publishAttempt)
		<-startDone
		t.Fatalf("ACK committed inside a stale lifecycle snapshot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(publishAttempt)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if err := <-ackDone; err != nil {
		t.Fatal(err)
	}
	current := waitStatus(t, scanner, Completed)
	if current.AckedThrough != 1 {
		t.Fatalf("attempt publication lost ACK: %+v", current)
	}
}

func TestAcknowledgementAloneNeverReleasesBatchesIncludingAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	scanner, db := newScanner(t, path, &localFactory{}, Options{})
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(ctx, localSpec(sourceFiles(t, 260))); err != nil {
		t.Fatal(err)
	}
	waitReleased(t, scanner, 1)
	if _, err := scanner.Ack(ctx, 1, 1, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.cleanupPage(ctx, 1); err != nil {
		t.Fatal(err)
	}
	assertBatches(t, scanner, 1, 260)
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, db = newScanner(t, path, &localFactory{}, Options{})
	if _, err := scanner.cleanupPage(ctx, 1); err != nil {
		t.Fatal(err)
	}
	assertBatches(t, scanner, 1, 260)
	current, err := scanner.Status(ctx, 1)
	if err != nil || current.AckedThrough != 3 || current.ReleasableThrough != 0 || current.ReleasedThrough != 0 || current.ReleaseProtocol != 0 {
		t.Fatalf("historical ACK became release permission: %+v %v", current, err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var format struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if _, err := tx.Get("metadata", "format", &format); err != nil {
			return err
		}
		if format.SchemaVersion != state.SchemaCompressedValues {
			t.Fatalf("ordinary ACK changed reader requirement: %d", format.SchemaVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRequiresAcknowledgedPrefixAndCurrentGenerationEpoch(t *testing.T) {
	ctx := context.Background()
	scanner, db, attempt := commitFixture(t)
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{scanTestBatch(0), scanTestBatch(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(ctx, 1, 1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unacknowledged release accepted: %v", err)
	}
	if _, err := scanner.Ack(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		generation uint64
		through    int64
		epoch      uint64
		want       error
	}{
		{1, -1, 0, ErrInvalid}, {1, 2, 0, ErrInvalid}, {2, 1, 0, ErrGeneration}, {1, 1, 1, ErrEpoch},
	} {
		if _, err := scanner.Release(ctx, 1, invalid.generation, invalid.through, invalid.epoch); !errors.Is(err, invalid.want) {
			t.Fatalf("release fence returned %v; want %v", err, invalid.want)
		}
	}
	assertBatches(t, scanner, 1, 256)
	if _, err := scanner.Release(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 1)
	if _, err := scanner.Release(ctx, 1, 1, 1); err != nil {
		t.Fatalf("duplicate release failed: %v", err)
	}
	if _, err := scanner.Ack(ctx, 1, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.cleanupPage(ctx, 1); err != nil {
		t.Fatal(err)
	}
	current, err := scanner.Status(ctx, 1)
	if err != nil || current.ReleasedThrough != 1 || current.ReleasableThrough != 1 || current.AckedThrough != 2 || current.LastBatch != 2 {
		t.Fatalf("ACK extended release permission: %+v %v", current, err)
	}
	assertBatchesFrom(t, scanner, 1, 1, 128)
}
