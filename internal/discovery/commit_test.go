package discovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func commitFixture(t *testing.T) (*Scanner, *state.Store, State) {
	t.Helper()
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
	attempt := State{Spec: localSpec(t.TempDir()), Generation: 1, Status: Scanning, Attempt: "scan-commit-test"}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put(states, parentKey(1), attempt) }); err != nil {
		t.Fatal(err)
	}
	return scanner, db, attempt
}

func TestScanCommitBoundsMetadataAndRetainsOversizedBatchOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var numbers []string
	writer := newScanCommitter(ctx, cancel, func(group []pendingBatch) error {
		var bytes int64
		for _, part := range group {
			bytes += batchMetadataBytes(part)
			numbers = append(numbers, part.objects[0].Key)
		}
		if len(group) > scanCommitBatches || len(group) > 1 && bytes > scanCommitMetadataBytes {
			t.Errorf("unbounded metadata group: %d batches, %d bytes", len(group), bytes)
		}
		return nil
	})
	for i, length := range []int{0, 400 << 10, 400 << 10, 2 << 20, 0, 0} {
		batch := pendingBatch{objects: []transferstore.Object{{Key: fmt.Sprint(i), Metadata: map[string]string{"value": strings.Repeat("\x00", length)}}}}
		if err := writer.add(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(numbers, ",") != "0,1,2,3,4,5" {
		t.Fatalf("metadata overflow changed batch order or lost a batch: %v", numbers)
	}
}

func scanTestBatch(number int) pendingBatch {
	batch := pendingBatch{objects: make([]transferstore.Object, 128)}
	for i := range batch.objects {
		batch.objects[i] = transferstore.Object{Key: fmt.Sprintf("%04d/%04d", number, i), Size: 9, ETag: "immutable"}
	}
	return batch
}

func TestScanGroupKeepsFirstBatchImmediateAndPublicBatchBoundaries(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commits := make(chan int, scanCommitBatches+1)
	writer := newScanCommitter(ctx, cancel, func(batch []pendingBatch) error {
		err := scanner.commitBatches(ctx, attempt, batch)
		commits <- len(batch)
		return err
	})
	if err := writer.add(scanTestBatch(0)); err != nil {
		t.Fatal(err)
	}
	first, found, err := scanner.Batch(ctx, 1, 1, 0)
	if err != nil || !found || len(first.Objects) != 128 || first.Number != 1 {
		t.Fatalf("first batch was not durable when add returned: %v", err)
	}
	for i := 1; i <= scanCommitBatches; i++ {
		if err := writer.add(scanTestBatch(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	close(commits)
	calls, grouped := 0, false
	for size := range commits {
		calls++
		grouped = grouped || size > 1
	}
	// Actual scheduling may split a burst at the 20ms deadline, but it must
	// coalesce some writes instead of persisting each public batch separately.
	if !grouped || calls >= scanCommitBatches+1 {
		t.Fatalf("%d scan batches used %d ungrouped transactions", scanCommitBatches+1, calls)
	}
	status, err := scanner.Status(ctx, 1)
	if err != nil || status.LastBatch != scanCommitBatches+1 || status.TotalObjects != (scanCommitBatches+1)*128 {
		t.Fatalf("grouping changed durable discovery totals: %+v, %v", status, err)
	}
	for i := int64(0); i < scanCommitBatches+1; i++ {
		batch, found, err := scanner.Batch(ctx, 1, 1, i)
		if err != nil || !found || batch.Number != i+1 || len(batch.Objects) != 128 {
			t.Fatalf("public batch %d changed: %+v %v", i+1, batch, err)
		}
	}
}

func TestSlowScanPublishesPendingBatchWithoutWaitingForMoreObjects(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan struct{}, 2)
	writer := newScanCommitter(ctx, cancel, func(batch []pendingBatch) error {
		err := scanner.commitBatches(ctx, attempt, batch)
		published <- struct{}{}
		return err
	})
	defer writer.close()
	if err := writer.add(scanTestBatch(0)); err != nil {
		t.Fatal(err)
	}
	<-published
	if err := writer.add(scanTestBatch(1)); err != nil {
		t.Fatal(err)
	}
	// No more scan callbacks or close occur while waiting. A synchronous-only
	// accumulator would leave this second batch unpublished indefinitely.
	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("a slow source held a completed metadata batch indefinitely")
	}
	status, err := scanner.Status(ctx, 1)
	if err != nil || status.TotalObjects != 256 || status.LastBatch != 2 {
		t.Fatalf("timed batch was not durable: %+v %v", status, err)
	}
}

func TestScanGroupedFailureDoesNotPublishPartialObjectsOrBatchNumbers(t *testing.T) {
	scanner, db, attempt := commitFixture(t)
	ctx := context.Background()
	first := scanTestBatch(0)
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{first}); err != nil {
		t.Fatal(err)
	}
	conflict := scanTestBatch(0)
	conflict.objects[0].Size++
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{scanTestBatch(1), conflict}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed source did not roll back its group: %v", err)
	}
	status, err := scanner.Status(ctx, 1)
	if err != nil || status.TotalObjects != 128 || status.LastBatch != 1 {
		t.Fatalf("a failed group exposed partial totals: %+v %v", status, err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var object discoveredObject
		found, err := tx.Get(objects, objectKey(1, scanTestBatch(1).objects[0].Key), &object)
		if found {
			t.Error("failed group left an object in the recovery inventory")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := scanner.Batch(ctx, 1, 1, 1); err != nil || found {
		t.Fatalf("failed group exposed a partially published batch: %v", err)
	}
}
