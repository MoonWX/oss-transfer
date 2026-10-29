package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func countCleanupRows(t *testing.T, db *state.Store, bucket string, parentID int64) int {
	t.Helper()
	count := 0
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(bucket, parentKey(parentID)+"/", "", 100000, func(_ string, _ json.RawMessage) error { count++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func waitReleased(t *testing.T, scanner *Scanner, parentID int64) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := scanner.Status(context.Background(), parentID)
		if err != nil {
			t.Fatal(err)
		}
		if current.ObjectsReleased {
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed scan did not release its recovery index")
	return State{}
}

func TestCompletedScanRetainsUnacknowledgedBatchesAndNeverRescans(t *testing.T) {
	ctx := context.Background()
	factory := &localFactory{}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(ctx, localSpec(sourceFiles(t, 260))); err != nil {
		t.Fatal(err)
	}
	current := waitReleased(t, scanner, 1)
	if !current.ScanComplete || current.TotalObjects != 260 || current.LastBatch != 3 {
		t.Fatalf("cleanup changed scan accounting: %+v", current)
	}
	if n := countCleanupRows(t, db, objects, 1); n != 0 {
		t.Fatalf("completed recovery index retained %d objects", n)
	}
	assertBatches(t, scanner, 1, 260)
	if _, err := scanner.Ack(ctx, 1, 1, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(ctx, 1, 1, 3); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 3)
	if _, err := scanner.Resume(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	if factory.opens.Load() != 1 || factory.scans.Load() != 1 {
		t.Fatal("resuming a completed scan reopened its source")
	}
	if _, _, err := scanner.Batch(ctx, 1, 2, 0); !errors.Is(err, ErrBatchReleased) {
		t.Fatalf("resume forgot released batch watermark: %v", err)
	}
	if _, found, err := scanner.Batch(ctx, 1, 2, 3); err != nil || found {
		t.Fatalf("read after the final batch: found=%v err=%v", found, err)
	}
}

func TestCleanupPreservesFailedAndInterruptedRecoveryIndexes(t *testing.T) {
	for _, scanErr := range []error{context.Canceled, errors.New("source failed")} {
		t.Run(scanErr.Error(), func(t *testing.T) {
			scanner, db, attempt := commitFixture(t)
			if err := scanner.commitBatches(context.Background(), attempt, []pendingBatch{scanTestBatch(0)}); err != nil {
				t.Fatal(err)
			}
			scanner.finish(attempt, scanErr)
			if _, err := scanner.cleanupPage(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if n := countCleanupRows(t, db, objects, 1); n != 128 {
				t.Fatalf("cleanup discarded the resumable inventory: %d", n)
			}
			assertBatches(t, scanner, 1, 128)
		})
	}
}

func TestCleanupResumesOldCompletedManifestsAfterPartialCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const total = cleanupPageKeys*2 + 13
	if err := db.Update(func(tx *state.Tx) error {
		current := State{Spec: localSpec(t.TempDir()), Generation: 1, Status: Completed, ScanComplete: true, TotalObjects: total, LastBatch: 1}
		if err := tx.Put(states, parentKey(1), current); err != nil {
			return err
		}
		// Deliberately omit the queue entry: older completed scans need the same cleanup.
		for i := 0; i < total; i++ {
			key := fmt.Sprintf("object-%05d", i)
			if err := tx.Put(objects, objectKey(1, key), discoveredObject{Object: transferstore.Object{Key: key, Size: 1}, SeenAttempt: "old-attempt"}); err != nil {
				return err
			}
		}
		return tx.Put(batches, batchKey(1, 1), Batch{ParentID: 1, Generation: 1, Number: 1, Objects: []transferstore.Object{{Key: "retained"}}})
	}); err != nil {
		t.Fatal(err)
	}
	manual := &Scanner{store: db, active: make(map[int64]*running)}
	if done, err := manual.cleanupPage(context.Background(), 1); err != nil || done {
		t.Fatalf("cleanup was not bounded: done=%v error=%v", done, err)
	}
	if n := countCleanupRows(t, db, objects, 1); total-n < 1 || total-n > cleanupPageKeys {
		t.Fatalf("first cleanup commit removed %d rows; limit %d", total-n, cleanupPageKeys)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	factory := &localFactory{}
	scanner, reopened := newScanner(t, path, factory, Options{})
	waitReleased(t, scanner, 1)
	if n := countCleanupRows(t, reopened, objects, 1); n != 0 {
		t.Fatalf("restarted cleanup left %d rows", n)
	}
	if n := countCleanupRows(t, reopened, batches, 1); n != 1 {
		t.Fatal("cleanup lost retained batch history")
	}
	if factory.opens.Load() != 0 {
		t.Fatal("startup cleanup opened the source")
	}
}

func TestPermanentDeleteFencesLiveScanBeforeCleaningAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{}), allowExit: make(chan struct{})}
	scanner, db := newScanner(t, path, factory, Options{})
	t.Cleanup(func() {
		select {
		case <-factory.allowExit:
		default:
			close(factory.allowExit)
		}
	})
	spec := localSpec(sourceFiles(t, 260))
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	before, err := scanner.Status(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := scanner.DeleteParent(short, 1, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete did not wait for the live source to close: %v", err)
	}
	if n := countCleanupRows(t, db, objects, 1); n != 128 {
		t.Fatal("cleanup raced the active scan")
	}
	if err := scanner.commitBatch(ctx, before, []transferstore.Object{{Key: "late", Size: 1}}); !errors.Is(err, ErrStopped) {
		t.Fatalf("late scan callback bypassed permanent fence: %v", err)
	}
	if _, err := scanner.Start(ctx, spec); !errors.Is(err, ErrDeleted) {
		t.Fatalf("late start bypassed permanent fence: %v", err)
	}
	if _, err := scanner.Resume(ctx, 1, 2); !errors.Is(err, ErrDeleted) {
		t.Fatalf("resume bypassed permanent fence: %v", err)
	}
	close(factory.allowExit)
	if err := scanner.DeleteParent(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{objects, batches} {
		if n := countCleanupRows(t, db, bucket, 1); n != 0 {
			t.Fatalf("purge retained %s: %d", bucket, n)
		}
	}
	current, err := scanner.Status(ctx, 1)
	if err != nil || !current.Deleted || !current.Purged || current.Spec.Source.Kind != "" || current.TotalObjects != 0 {
		t.Fatalf("purge did not retain a compact tombstone: %+v %v", current, err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := newScanner(t, path, &localFactory{}, Options{})
	if err := reopened.DeleteParent(ctx, 1, 1); err != nil {
		t.Fatal("replayed deletion failed", err)
	}
	spec.Generation = 9
	if _, err := reopened.Start(ctx, spec); !errors.Is(err, ErrDeleted) {
		t.Fatalf("restart lost permanent fence: %v", err)
	}
	if _, err := reopened.Resume(ctx, 1, 9); !errors.Is(err, ErrDeleted) {
		t.Fatalf("restart allowed a deleted scan to resume: %v", err)
	}
	if _, _, err := reopened.Batch(ctx, 1, 1, 0); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted batch reported a corrupt manifest instead of deletion: %v", err)
	}
}

func TestDeleteUnknownScanFencesFutureStartAndPreservesOtherParents(t *testing.T) {
	ctx := context.Background()
	scanner, db, attempt := commitFixture(t)
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{scanTestBatch(0)}); err != nil {
		t.Fatal(err)
	}
	if err := scanner.DeleteParent(ctx, 2, 3); err != nil {
		t.Fatal(err)
	}
	spec := localSpec(t.TempDir())
	spec.ParentID = 2
	spec.Generation = 3
	if _, err := scanner.Start(ctx, spec); !errors.Is(err, ErrDeleted) {
		t.Fatalf("unknown scan deletion did not fence later start: %v", err)
	}
	if err := scanner.DeleteParent(ctx, 2, 2); !errors.Is(err, ErrGeneration) {
		t.Fatalf("older delete advanced fence: %v", err)
	}
	if n := countCleanupRows(t, db, objects, 1); n != 128 {
		t.Fatalf("unrelated live recovery records changed: %d", n)
	}
	assertBatches(t, scanner, 1, 128)
}

func TestCleanupUsesEncodedBytesAndRetainsOtherPrefixes(t *testing.T) {
	_, db, _ := commitFixture(t)
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	const bucket = "cleanup-byte-fixture"
	if err := db.Update(func(tx *state.Tx) error {
		for key, value := range map[string]string{"1/a": strings.Repeat("x", cleanupPageBytes+1), "1/b": "remaining", "2/a": "other-parent"} {
			if err := tx.Put(bucket, key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		done, err := deleteCleanupPage(tx, bucket, "1/")
		if !done {
			t.Fatal("key-only cleanup inflated a highly compressed value")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var value string
		if found, err := tx.Get(bucket, "1/a", &value); err != nil || found {
			t.Fatalf("oversized first row made no progress: %v", err)
		}
		for _, key := range []string{"2/a"} {
			if found, err := tx.Get(bucket, key, &value); err != nil || !found {
				t.Fatalf("cleanup deleted beyond its page or prefix: %s %v", key, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPermanentDeleteReleasesLegacyReconciliationManifest(t *testing.T) {
	scanner, db, attempt := commitFixture(t)
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put(legacyManifest, parentKey(1), LegacyManifest{ParentID: 1, MigrationID: "old", Sealed: true}); err != nil {
			return err
		}
		for _, id := range []int64{1, 2} {
			if err := tx.Put(legacyKeys, objectKey(id, "key"), LegacyKey{Key: "key"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := scanner.DeleteParent(context.Background(), 1, attempt.Generation); err != nil {
		t.Fatal(err)
	}
	if n := countCleanupRows(t, db, legacyKeys, 1); n != 0 {
		t.Fatalf("purge retained %d legacy rows", n)
	}
	if n := countCleanupRows(t, db, legacyKeys, 2); n != 1 {
		t.Fatal("purge deleted another task's legacy rows")
	}
	if err := db.View(func(tx *state.Tx) error {
		var header LegacyManifest
		if found, err := tx.Get(legacyManifest, parentKey(1), &header); err != nil || found {
			t.Fatalf("purge retained legacy header: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
