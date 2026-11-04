package execution

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// barrierTarget records which deferred writes each Sync covered and checks,
// at every barrier, that no result the ledger already holds is still owed one.
type barrierTarget struct {
	*transferstore.Local
	t       *testing.T
	repo    *Repository
	child   string
	fail    bool
	mu      sync.Mutex
	written []string        // deferred writes returned since the last Sync
	synced  map[string]bool // destination keys covered by a completed Sync
	syncs   int
	unsafe  []string
}

func (b *barrierTarget) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if !options.DeferDurability {
		b.t.Errorf("engine wrote %s without deferring durability", key)
	}
	result, err := b.Local.Write(ctx, key, input, options)
	if err == nil && result.Committed {
		b.mu.Lock()
		b.written = append(b.written, key)
		b.mu.Unlock()
	}
	return result, err
}

func (b *barrierTarget) Sync(ctx context.Context) error {
	b.checkLedger()
	b.mu.Lock()
	written := append([]string(nil), b.written...)
	b.written = b.written[:0]
	b.syncs++
	b.mu.Unlock()
	if b.fail {
		return errors.New("injected directory sync failure")
	}
	if err := b.Local.Sync(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	for _, key := range written {
		b.synced[key] = true
	}
	b.mu.Unlock()
	return nil
}

// checkLedger fails if a succeeded result was committed before its barrier.
func (b *barrierTarget) checkLedger() {
	record, err := b.repo.Get(bg, b.child)
	if err != nil {
		b.t.Error(err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, result := range record.Results {
		if result.Status == ItemSucceeded && !b.synced[result.Target.Key] {
			b.unsafe = append(b.unsafe, result.Target.Key)
		}
	}
}

func newBarrierFixture(t *testing.T, count int, fail bool) (*fixture, *barrierTarget) {
	f := newFixture(t, count)
	target := &barrierTarget{t: t, repo: f.repo, child: f.job.ChildID, fail: fail, synced: make(map[string]bool)}
	factory := StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		source, destination, err := f.factory().Open(ctx, job)
		if err != nil {
			return nil, nil, err
		}
		target.Local = destination.(*transferstore.Local)
		return source, target, nil
	})
	// Small batches exercise the periodic flush as well as the final one.
	f.startEngine(factory, Options{CommitBatchSize: 8, CommitInterval: time.Hour, WorkersPerJob: 4})
	return f, target
}

func TestEngineSyncsDeferredWritesBeforeCommittingResults(t *testing.T) {
	f, target := newBarrierFixture(t, 40, false)
	f.acceptStart()
	record := f.wait()
	if record.Status != Completed || record.Counters.Succeeded != 40 {
		t.Fatalf("unexpected record: %+v", record.Counters)
	}
	f.assertFiles()
	target.checkLedger()
	if len(target.unsafe) != 0 {
		t.Fatalf("results committed before their durability barrier: %v", target.unsafe)
	}
	// One barrier per committed batch, not one per object.
	if target.syncs < 2 || target.syncs > 6 {
		t.Fatalf("expected one barrier per result batch, got %d", target.syncs)
	}
}

func TestEngineSyncFailureCommitsNoUnsyncedResult(t *testing.T) {
	f, target := newBarrierFixture(t, 20, true)
	f.acceptStart()
	record := f.wait()
	if record.Status == Completed || record.Counters.Succeeded != 0 {
		t.Fatalf("results were committed although the barrier failed: %s %+v", record.Status, record.Counters)
	}
	if target.syncs == 0 {
		t.Fatal("engine never requested a durability barrier")
	}
}
