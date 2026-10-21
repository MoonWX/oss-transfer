package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestFinalResultsAndCompletedStateUseOneDurableCommit(t *testing.T) {
	f := newFixture(t, 128)
	observed := &observedDB{db: f.db, child: f.job.ChildID}
	f.repo, _ = NewRepository(observed, RepositoryOptions{})
	// Keep the size threshold at its production default, but prevent a slow
	// test filesystem from firing the independent progress timer.
	f.startEngine(nil, Options{CommitInterval: time.Hour})
	f.acceptStart()
	completed := f.wait()
	if completed.Status != Completed || completed.Counters.Succeeded != 128 {
		t.Fatalf("unexpected final result: %+v", completed.Counters)
	}
	if got := observed.updates.Load(); got != 3 {
		t.Fatalf("accept, begin and final results must use three transactions; got %d", got)
	}
	f.assertFiles()
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.engine = nil
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, _ = NewRepository(f.db, RepositoryOptions{})
	reopened, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || !reflect.DeepEqual(completed, reopened) {
		t.Fatalf("final state and results must survive reopen together: %v", err)
	}
}

type lastObjectBlockedStore struct {
	transferstore.Store
	key     string
	entered chan struct{}
	release chan struct{}
}

func (s *lastObjectBlockedStore) Write(ctx context.Context, key string, input io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if key == s.key {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return transferstore.WriteResult{}, ctx.Err()
		}
	}
	return s.Store.Write(ctx, key, input, opts)
}

func TestProgressTimerStillPersistsPartialBatch(t *testing.T) {
	f := newFixture(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		source, target, err := base.Open(ctx, j)
		return source, &lastObjectBlockedStore{Store: target, key: j.Items[1].DestinationKey, entered: entered, release: release}, err
	}), Options{WorkersPerJob: 1})
	f.acceptStart()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second transfer did not reach the blocking point")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := f.repo.Get(bg, f.job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Counters.Succeeded == 1 {
			if r.Status != Running || r.Results[1].Status != ItemPending {
				t.Fatalf("partial batch falsely completed the child: %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("result below the size threshold was not persisted by the progress timer")
		}
		time.Sleep(5 * time.Millisecond)
	}
	once.Do(func() { close(release) })
	if done := f.wait(); done.Status != Completed || done.Counters.Succeeded != 2 {
		t.Fatalf("unexpected completed child: %+v", done)
	}
}

func TestRepeatedSealsStopsAndResultsPreserveUpdatedAt(t *testing.T) {
	f := newFixture(t, 1)
	accepted, err := f.repo.Accept(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.repo.Seal(bg, f.job.ChildID, 1)
	if err != nil || !reflect.DeepEqual(accepted, sealed) {
		t.Fatalf("a repeated seal rewrote the assignment: %v", err)
	}
	running, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	results := []ItemResult{{ID: f.job.Items[0].ID, Status: ItemSkipped, Target: f.job.Items[0].Source}}
	first, err := f.repo.commit(bg, f.job.ChildID, 1, running.Attempt, results)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.repo.commit(bg, f.job.ChildID, 1, running.Attempt, results)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("duplicate results rewrote the record: %v", err)
	}
	stopped, err := f.repo.stop(bg, f.job.ChildID, 1)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := f.repo.stop(bg, f.job.ChildID, 1)
	if err != nil || !reflect.DeepEqual(stopped, repeated) {
		t.Fatalf("duplicate stop rewrote the record: %v", err)
	}
}

type ledgerWriteMeter struct {
	db      *state.Store
	child   string
	writes  atomic.Int64
	payload atomic.Int64
}

func (d *ledgerWriteMeter) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *ledgerWriteMeter) Update(fn func(*state.Tx) error) error {
	var bytes int64
	err := d.db.Update(func(tx *state.Tx) error {
		before, _ := load(tx, d.child)
		if err := fn(tx); err != nil {
			return err
		}
		after, _ := load(tx, d.child)
		if !reflect.DeepEqual(before, after) {
			encoded, err := json.Marshal(state.ForStorage(after))
			if err != nil {
				return err
			}
			bytes = int64(len(encoded))
		}
		return nil
	})
	if err == nil && bytes > 0 {
		d.writes.Add(1)
		d.payload.Add(bytes)
	}
	return err
}

// This measures durable Agent result bookkeeping only. Target data transfer,
// network delay and filesystem write amplification are intentionally excluded;
// record bytes are the JSON values written, not physical disk traffic.
func BenchmarkResultLedgerBatching(b *testing.B) {
	for _, mode := range []string{"previous_32_then_finish", "combined_128_and_finish"} {
		b.Run(mode, func(b *testing.B) {
			db, err := state.Open(filepath.Join(b.TempDir(), "agent.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			meter := &ledgerWriteMeter{db: db}
			repo, err := NewRepository(meter, RepositoryOptions{})
			if err != nil {
				b.Fatal(err)
			}
			items, results := make([]Item, 128), make([]ItemResult, 128)
			for i := range items {
				key := strings.Repeat("folder/", 8) + fmt.Sprintf("object-%04d.bin", i)
				obj := transferstore.Object{Key: key, Size: 4096, ETag: strings.Repeat("a", 32)}
				items[i] = Item{ID: fmt.Sprint(i), Source: obj, DestinationKey: key}
				results[i] = ItemResult{ID: items[i].ID, Status: ItemSucceeded, Target: obj, TargetCommitted: true}
			}
			var writes, payload int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				job := Job{ParentID: int64(i + 1), Number: 1, ChildID: fmt.Sprintf("%d-1", i+1), Generation: 1, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: items, Source: Endpoint{Kind: "S3", Ref: "source", Bucket: "source"}, Destination: Endpoint{Kind: "S3", Ref: "destination", Bucket: "destination"}}
				meter.child = job.ChildID
				if _, err := repo.Accept(bg, job); err != nil {
					b.Fatal(err)
				}
				running, err := repo.begin(bg, job.ChildID, 1, false, false)
				if err != nil {
					b.Fatal(err)
				}
				meter.writes.Store(0)
				meter.payload.Store(0)
				b.StartTimer()
				if mode == "previous_32_then_finish" {
					for offset := 0; offset < len(results); offset += 32 {
						if _, err := repo.commit(bg, job.ChildID, 1, running.Attempt, results[offset:offset+32]); err != nil {
							b.Fatal(err)
						}
					}
					_, err = repo.finish(bg, job.ChildID, 1, running.Attempt, nil)
				} else {
					_, err = repo.finish(bg, job.ChildID, 1, running.Attempt, nil, results...)
				}
				if err != nil {
					b.Fatal(err)
				}
				writes += meter.writes.Load()
				payload += meter.payload.Load()
			}
			b.ReportMetric(float64(writes)/float64(b.N), "commits/child")
			b.ReportMetric(float64(payload)/float64(b.N), "record-bytes/child")
		})
	}
}
