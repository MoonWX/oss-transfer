package execution

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestTryRecoverPreservesLedgerAndAdmissionBounds(t *testing.T) {
	f := newFixture(t, 3)
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	record, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// Persist one genuine completed target before the simulated shutdown.
	source, target, err := f.factory().Open(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	first := f.job.Items[0]
	input, _, err := source.Open(bg, first.Source.Key, transferstore.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Write(bg, first.DestinationKey, input, transferstore.WriteOptions{Size: first.Source.Size, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	input.Close()
	object, err := target.Stat(bg, first.DestinationKey)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	target.Close()
	if _, err = f.repo.commit(bg, f.job.ChildID, 1, record.Attempt, []ItemResult{{ID: first.ID, Status: ItemSucceeded, Target: object}}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.finish(bg, f.job.ChildID, 1, record.Attempt, context.Canceled); err != nil {
		t.Fatal(err)
	}
	before, _ := f.repo.Get(bg, f.job.ChildID)
	gate := make(chan struct{})
	blockerGate := make(chan struct{})
	var opens atomic.Int64
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		opens.Add(1)
		waitFor := gate
		if job.ChildID != f.job.ChildID {
			waitFor = blockerGate
		}
		select {
		case <-waitFor:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return f.factory().Open(ctx, job)
	}), Options{MaxConcurrentJobs: 1})
	// Another real child owns the sole execution slot. A manual request must
	// not block or change the interrupted job's attempt/counters.
	blocker := f.job
	blocker.ChildID, blocker.Number = "224-2", 2
	blocker.Items = append([]Item(nil), f.job.Items...)
	for i := range blocker.Items {
		blocker.Items[i].DestinationKey = "other/" + blocker.Items[i].DestinationKey
	}
	if _, err := f.engine.Accept(bg, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(bg, blocker.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); !errors.Is(err, ErrCapacity) || time.Since(start) > time.Second {
		t.Fatalf("full pool did not return immediately: %v", err)
	}
	after, _ := f.repo.Get(bg, f.job.ChildID)
	if after.Attempt != before.Attempt || after.Counters != before.Counters || after.Status != Recoverable {
		t.Fatalf("waiting changed ledger: before=%+v after=%+v", before, after)
	}
	close(blockerGate)
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if record, err := f.engine.Wait(ctx, blocker.ChildID); err != nil || record.Status != Completed {
		t.Fatal("slot owner did not complete", record, err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 2); !errors.Is(err, ErrGeneration) {
		t.Fatalf("stale generation was admitted: %v", err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); err != nil {
		t.Fatal("duplicate click was not idempotent", err)
	}
	after, _ = f.repo.Get(bg, f.job.ChildID)
	if after.Attempt != before.Attempt+1 || after.Counters != before.Counters {
		t.Fatal("recovery reset progress or created a second attempt", after)
	}
	close(gate)
	finished := f.wait()
	if finished.Status != Completed || finished.Counters.Succeeded != 3 || opens.Load() != 2 {
		t.Fatal("recovery did not finish exactly one execution", finished, opens.Load())
	}
	f.assertFiles()
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); !errors.Is(err, ErrNotRecoverable) {
		t.Fatalf("finished child was restarted: %v", err)
	}
}

func TestTryRecoverCannotUndoStopFence(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.StopParent(bg, f.job.ParentID, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); !errors.Is(err, ErrNotRecoverable) && !errors.Is(err, ErrStopped) {
		t.Fatalf("stop fence was bypassed: %v", err)
	}
	record, _ := f.repo.Get(bg, f.job.ChildID)
	if record.Status != Stopped || record.Counters.Succeeded != 0 {
		t.Fatal("manual recovery changed a stopped job", record)
	}
}
