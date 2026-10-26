package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func takeoverObject(t *testing.T, root, key string) transferstore.Object {
	t.Helper()
	s, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	object, err := s.Stat(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func takeoverBatch(number int64, items ...transferstore.Object) Batch {
	batch := Batch{ParentID: 1, Generation: 1, Number: number, Objects: items}
	for _, item := range items {
		batch.Bytes += item.Size
	}
	return batch
}

func takeoverRequest(spec Spec, batches ...Batch) TakeoverRequest {
	request := TakeoverRequest{Spec: spec, Epoch: 1, ThroughBatch: int64(len(batches)), Batches: batches}
	for _, batch := range batches {
		request.Objects += int64(len(batch.Objects))
		request.Bytes += batch.Bytes
	}
	return request
}

func seedTakeover(t *testing.T, scanner *Scanner, request TakeoverRequest) State {
	t.Helper()
	for range 32 {
		current, err := scanner.SeedTakeover(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if !current.Takeover.Cleaning {
			return current
		}
	}
	t.Fatal("takeover cleanup did not complete")
	return State{}
}

func takeoverNewKeys(t *testing.T, scanner *Scanner, request TakeoverRequest) []string {
	t.Helper()
	var keys []string
	after := request.ThroughBatch
	for {
		batch, found, err := scanner.Batch(context.Background(), request.Spec.ParentID, request.Spec.Generation, after, request.Epoch)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return keys
		}
		if batch.Number != after+1 || batch.Epoch != request.Epoch {
			t.Fatal("takeover batch numbering/epoch changed", batch)
		}
		for _, item := range batch.Objects {
			keys = append(keys, item.Key)
		}
		after = batch.Number
	}
}

func TestTakeoverReplaysNonMonotonicLocalHistoryAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, key := range []string{"a/child", "a.txt", "b", "z"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, key)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, key), []byte("body"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	z, a := takeoverObject(t, root, "z"), takeoverObject(t, root, "a/child")
	// A shared filesystem may report a different device/inode identity here.
	z.ETag, a.ETag = "local:previous-mount-z", "local:previous-mount-a"
	request := takeoverRequest(localSpec(root), takeoverBatch(1, z), takeoverBatch(2, a), takeoverBatch(3, z))
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &localFactory{}
	scanner, db := newScanner(t, path, factory, Options{MaxBatchObjects: 1})
	first := request
	first.Batches = first.Batches[:1]
	current := seedTakeover(t, scanner, first)
	if current.Takeover.SeededThrough != 1 || factory.opens.Load() != 0 {
		t.Fatal("seed unexpectedly scanned source", current)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); !errors.Is(err, ErrSeeding) {
		t.Fatal("incomplete manifest started", err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, _ = newScanner(t, path, factory, Options{MaxBatchObjects: 1})
	// Replaying the lost response must not count batch 1 twice.
	current = seedTakeover(t, scanner, request)
	if current.Takeover.SeededObjects != 3 || current.Takeover.DuplicateObjects != 1 {
		t.Fatal("seed retry/duplicate key changed totals", current.Takeover)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	current = waitStatus(t, scanner, Completed)
	if current.TotalObjects != 5 || current.TotalBytes != 20 || current.SeenObjects != 5 || current.MissingObjects != 0 || current.LastBatch != 5 || current.AckedThrough != 3 {
		t.Fatal("takeover lost its assigned prefix or counted it twice", current)
	}
	if keys := takeoverNewKeys(t, scanner, request); !reflect.DeepEqual(keys, []string{"a.txt", "b"}) {
		t.Fatal("history order or local traversal lost/duplicated keys", keys)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil || factory.scans.Load() != 1 {
		t.Fatal("finalize replay restarted scan", err, factory.scans.Load())
	}
	if _, _, err := scanner.Batch(ctx, 1, 1, 0, 1); !errors.Is(err, ErrSeeding) {
		t.Fatal("missing historical batches masqueraded as an empty result", err)
	}
	changed := request
	changed.Batches = append([]Batch(nil), request.Batches[:1]...)
	changed.Batches[0].Objects = append([]transferstore.Object(nil), changed.Batches[0].Objects...)
	changed.Batches[0].Objects[0].ETag = "changed"
	if _, err := scanner.SeedTakeover(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay was accepted", err)
	}
}

func TestTakeoverSingleKeyFiltersAndMissingAssignedSource(t *testing.T) {
	for _, mode := range []string{"single", "range", "date", "missing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := sourceFiles(t, 3)
			first := takeoverObject(t, root, "0000.txt")
			spec := localSpec(root)
			switch mode {
			case "single":
				spec.SingleKey = first.Key
			case "range":
				spec.StartKey, spec.EndKey = "0000.txt", "0001.txt"
				first = takeoverObject(t, root, "0001.txt")
			case "date":
				now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
				if err := os.Chtimes(filepath.Join(root, "0000.txt"), now, now); err != nil {
					t.Fatal(err)
				}
				first = takeoverObject(t, root, "0000.txt")
				spec.ModifiedFrom, spec.ModifiedUntil = now.Add(-time.Hour), now.Add(time.Hour)
			case "missing":
				if err := os.Remove(filepath.Join(root, first.Key)); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.WriteFile(filepath.Join(root, first.Key), []byte("changed bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
			request := takeoverRequest(spec, takeoverBatch(1, first))
			seedTakeover(t, scanner, request)
			if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
				t.Fatal(err)
			}
			if mode == "changed" {
				got := waitStatus(t, scanner, Failed)
				if !strings.Contains(got.Error, ErrSourceChanged.Error()) {
					t.Fatal(got)
				}
				return
			}
			got := waitStatus(t, scanner, Completed)
			keys := takeoverNewKeys(t, scanner, request)
			if mode == "missing" {
				if got.TotalObjects != 3 || got.MissingObjects != 1 || len(keys) != 2 {
					t.Fatal(got, keys)
				}
			} else if got.TotalObjects != 1 || got.MissingObjects != 0 || len(keys) != 0 {
				t.Fatal("filter/single-key takeover expanded the scope", got, keys)
			}
		})
	}
}

func TestTakeoverInventoryRetainsExactKeysAndRequiresSeal(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 4)
	factory := &localFactory{}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	descriptor := inventory.Descriptor{ID: inventorySHA("takeover"), Name: "keys", ContentSHA256: inventorySHA("0002.txt\nmissing\n0000.txt\n"), Binding: inventorySHA("binding"), SpecSHA256: inventorySHA("spec"), ParentID: 1}
	spec := localSpec(root)
	spec.InventoryID = descriptor.ID
	request := takeoverRequest(spec, takeoverBatch(1, takeoverObject(t, root, "0002.txt")))
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatal(err)
	}
	repo := inventory.New(db)
	if _, err := repo.Begin(ctx, descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Put(ctx, inventory.Batch{ID: descriptor.ID, Number: 1, Keys: []string{"0002.txt", "missing", "0000.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); !errors.Is(err, inventory.ErrUnsealed) {
		t.Fatal(err)
	}
	if _, err := repo.Seal(ctx, inventory.Seal{ID: descriptor.ID, BatchCount: 1, UniqueCount: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, scanner, Completed)
	keys := takeoverNewKeys(t, scanner, request)
	slices.Sort(keys)
	if got.TotalObjects != 3 || got.MissingObjects != 0 || factory.scans.Load() != 0 || !reflect.DeepEqual(keys, []string{"0000.txt", "missing"}) {
		t.Fatal("takeover scanned outside inventory or lost missing item", got, keys)
	}
}

func TestTakeoverCleanupIsBoundedAndResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	scanner, db := newScanner(t, path, &localFactory{}, Options{})
	root := sourceFiles(t, 1)
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put(states, parentKey(1), State{Spec: localSpec(root), Generation: 1, Status: Stopped}); err != nil {
			return err
		}
		for index := range cleanupPageKeys + 200 {
			key := fmt.Sprintf("old-%06d", index)
			if err := tx.Put(objects, objectKey(1, key), discoveredObject{Object: transferstore.Object{Key: key}, SeenAttempt: "old"}); err != nil {
				return err
			}
		}
		return tx.Put(objects, objectKey(2, "keep"), discoveredObject{Object: transferstore.Object{Key: "keep"}})
	}); err != nil {
		t.Fatal(err)
	}
	request := takeoverRequest(localSpec(root))
	request.Spec.Generation = 2 // This Agent last scanned an older task generation.
	current, err := scanner.SeedTakeover(ctx, request)
	if err != nil || !current.Takeover.Cleaning || countCleanupRows(t, db, objects, 1) != 200 {
		t.Fatal(current, err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, db = newScanner(t, path, &localFactory{}, Options{})
	seedTakeover(t, scanner, request)
	if countCleanupRows(t, db, objects, 1) != 0 || countCleanupRows(t, db, objects, 2) != 1 {
		t.Fatal("cleanup crossed parent boundary")
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 2, 1); err != nil {
		t.Fatal(err)
	}
	if got := waitStatus(t, scanner, Completed); got.TotalObjects != 1 {
		t.Fatal(got)
	}
}

func TestTakeoverFencesOldEpochAndCancelsOnParentLease(t *testing.T) {
	ctx := context.Background()
	lease, cancel := context.WithCancel(ctx)
	defer cancel()
	root := sourceFiles(t, 2)
	factory := &localFactory{gateAfter: 1, reached: make(chan struct{}), release: make(chan struct{})}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{ParentContext: func(int64, uint64) (context.Context, error) { return lease, lease.Err() }})
	request := takeoverRequest(localSpec(root))
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	for name, action := range map[string]func() error{
		"start":  func() error { _, err := scanner.Start(ctx, localSpec(root)); return err },
		"stop":   func() error { _, err := scanner.Stop(ctx, 1, 1, 0); return err },
		"resume": func() error { _, err := scanner.Resume(ctx, 1, 2, 0); return err },
		"batch":  func() error { _, _, err := scanner.Batch(ctx, 1, 1, 0, 0); return err },
		"ack":    func() error { _, err := scanner.Ack(ctx, 1, 1, 0, 0); return err },
		"status": func() error { _, err := scanner.StatusEpoch(ctx, 1, 1, 0); return err },
	} {
		if err := action(); !errors.Is(err, ErrEpoch) {
			t.Fatalf("stale %s command accepted: %v", name, err)
		}
	}
	cancel()
	if err := scanner.QuiesceParent(ctx, 1); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, scanner, Recoverable)
	request.Spec.Epoch = 1
	if _, err := scanner.Start(ctx, request.Spec); !errors.Is(err, context.Canceled) {
		t.Fatal("expired lease restarted scan", err)
	}
}

type takeoverS3Factory struct{ store transferstore.Store }

func (f takeoverS3Factory) OpenEndpoint(context.Context, execution.Endpoint) (transferstore.Store, error) {
	return f.store, nil
}

func TestTakeoverS3ReplaysMetadataWithoutReadingBodies(t *testing.T) {
	var lists atomic.Int32
	modified := time.Date(2026, 10, 21, 7, 28, 0, 0, time.UTC)
	source := metadataTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			t.Error("takeover read object data", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lists.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>a</Key><Size>4</Size><ETag>"a"</ETag><LastModified>2026-10-21T07:28:00Z</LastModified></Contents><Contents><Key>z</Key><Size>4</Size><ETag>"z"</ETag><LastModified>2026-10-21T07:28:00Z</LastModified></Contents></ListBucketResult>`)
	})
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), takeoverS3Factory{source}, Options{})
	spec := Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "inventory"}}
	request := takeoverRequest(spec, takeoverBatch(1, transferstore.Object{Key: "z", Size: 4, ETag: `"z"`, ModifiedAt: modified}))
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, scanner, Completed)
	if got.TotalObjects != 2 || lists.Load() != 1 || !reflect.DeepEqual(takeoverNewKeys(t, scanner, request), []string{"a"}) {
		t.Fatal(got, lists.Load())
	}
}

func TestTakeoverStopAndResumeDuringSeedKeepsDurableCursor(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 3)
	factory := &localFactory{}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	request := takeoverRequest(localSpec(root), takeoverBatch(1, takeoverObject(t, root, "0002.txt")), takeoverBatch(2, takeoverObject(t, root, "0000.txt")))
	first := request
	first.Batches = first.Batches[:1]
	seedTakeover(t, scanner, first)
	if _, err := scanner.Stop(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if _, err := scanner.SeedTakeover(ctx, request); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	current, err := scanner.Resume(ctx, 1, 2, 1)
	if err != nil || current.Status != Seeding || current.Takeover.SeededThrough != 1 || factory.opens.Load() != 0 {
		t.Fatal(current, err)
	}
	request.Spec.Generation = 2
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(ctx, 1, 2, 1); err != nil {
		t.Fatal(err)
	}
	current = waitStatus(t, scanner, Completed)
	if current.TotalObjects != 3 || !reflect.DeepEqual(takeoverNewKeys(t, scanner, request), []string{"0001.txt"}) {
		t.Fatal(current)
	}
}

func TestTakeoverCannotClearActiveScanBeforeItQuiesces(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 2)
	factory := &localFactory{gateAfter: 1, reached: make(chan struct{}), release: make(chan struct{}), allowExit: make(chan struct{})}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{MaxBatchObjects: 1})
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	request := takeoverRequest(localSpec(root), takeoverBatch(1, takeoverObject(t, root, "0000.txt")))
	if _, err := scanner.SeedTakeover(ctx, request); !errors.Is(err, ErrRunning) {
		t.Fatal(err)
	}
	current, err := scanner.Status(ctx, 1)
	if err != nil || current.Epoch != 0 || current.LastBatch != 1 || current.Takeover != nil {
		t.Fatal("live manifest was erased", current, err)
	}
	close(factory.allowExit)
	if err := scanner.QuiesceParent(ctx, 1); err != nil {
		t.Fatal(err)
	}
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	current = waitStatus(t, scanner, Completed)
	if current.TotalObjects != 2 || !reflect.DeepEqual(takeoverNewKeys(t, scanner, request), []string{"0001.txt"}) {
		t.Fatal(current)
	}
}

func TestTakeoverSeedRejectsConflictingDuplicatesAtomically(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 2)
	factory := &localFactory{}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	object := takeoverObject(t, root, "0000.txt")
	changed := object
	changed.ETag = "different-version"
	request := takeoverRequest(localSpec(root), takeoverBatch(1, object), takeoverBatch(2, changed))
	if _, err := scanner.SeedTakeover(ctx, request); !errors.Is(err, ErrSourceChanged) {
		t.Fatal(err)
	}
	if _, err := scanner.Status(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial seed state committed", err)
	}
	if countCleanupRows(t, db, objects, 1) != 0 || factory.opens.Load() != 0 {
		t.Fatal("partial seed keys escaped failed page")
	}
	request.Spec.LegacyManifestID = "legacy"
	if _, err := scanner.SeedTakeover(ctx, request); !errors.Is(err, ErrInvalid) {
		t.Fatal("legacy two-phase source accepted", err)
	}
}

type leaseCancelFactory struct {
	root              string
	started, canceled chan struct{}
}
type leaseCancelStore struct {
	transferstore.Store
	factory *leaseCancelFactory
}

func (f *leaseCancelFactory) OpenEndpoint(context.Context, execution.Endpoint) (transferstore.Store, error) {
	source, err := transferstore.NewLocal(f.root)
	return &leaseCancelStore{Store: source, factory: f}, err
}
func (s *leaseCancelStore) Scan(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Object) error) error {
	return s.Store.Scan(ctx, options, func(object transferstore.Object) error {
		if err := emit(object); err != nil {
			return err
		}
		close(s.factory.started)
		<-ctx.Done()
		close(s.factory.canceled)
		return ctx.Err()
	})
}

func TestScannerLeaseCancelsSourceWhileStateWriterIsBusy(t *testing.T) {
	ctx := context.Background()
	lease, cancel := context.WithCancel(ctx)
	defer cancel()
	factory := &leaseCancelFactory{root: sourceFiles(t, 1), started: make(chan struct{}), canceled: make(chan struct{})}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{ParentContext: func(int64, uint64) (context.Context, error) { return lease, lease.Err() }})
	if _, err := scanner.Start(ctx, localSpec(factory.root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.started)
	locked, unlock, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { written <- db.Update(func(*state.Tx) error { close(locked); <-unlock; return nil }) }()
	waitReached(t, locked)
	cancel()
	select {
	case <-factory.canceled:
	case <-time.After(time.Second):
		close(unlock)
		<-written
		t.Fatal("source cancellation waited for the database writer")
	}
	close(unlock)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := scanner.QuiesceParent(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if current := waitStatus(t, scanner, Recoverable); current.LastBatch != 0 {
		t.Fatal("expired scan published its pending batch", current)
	}
}
