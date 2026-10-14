package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/inventory"
)

func inventorySHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func TestInventoryRequiresSealedMatchingParentAndNeverScansBucket(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	f := &localFactory{}
	scanner, db := newScanner(t, path, f, Options{})
	repo := inventory.New(db)
	d := inventory.Descriptor{ID: inventorySHA("manifest"), Name: "keys.incr", ContentSHA256: inventorySHA("0000.txt\nmissing\n0000.txt\n"), Binding: inventorySHA("binding"), SpecSHA256: inventorySHA("spec"), ParentID: 1}
	spec := localSpec(sourceFiles(t, 3))
	spec.InventoryID = d.ID
	if _, err := scanner.Start(ctx, spec); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := repo.Begin(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(ctx, spec); !errors.Is(err, inventory.ErrUnsealed) {
		t.Fatal("unsealed scan started", err)
	}
	if f.opens.Load() != 0 {
		t.Fatal("opened source before seal")
	}
	if _, err := repo.Put(ctx, inventory.Batch{ID: d.ID, Number: 1, Keys: []string{"0000.txt", "missing", "0000.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Seal(ctx, inventory.Seal{ID: d.ID, BatchCount: 1, UniqueCount: 2}); err != nil {
		t.Fatal(err)
	}
	wrong := spec
	wrong.ParentID = 2
	if _, err := scanner.Start(ctx, wrong); !errors.Is(err, inventory.ErrConflict) {
		t.Fatal("wrong parent accepted", err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, _ = newScanner(t, path, f, Options{})
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, scanner, Completed)
	if got.TotalObjects != 2 || f.scans.Load() != 0 || f.stats.Load() != 2 {
		t.Fatal("inventory expanded or dropped missing key", got, f.scans.Load(), f.stats.Load())
	}
	assertBatches(t, scanner, 1, 2)
	batch, found, err := scanner.Batch(ctx, 1, 1, 0)
	if err != nil || !found || len(batch.Legacy) != 0 {
		t.Fatal("new inventory acquired legacy provenance", batch, found, err)
	}
}

// Pause metadata lookup only after the first durable batch. The real local store
// still supplies all object identities; interruption cannot consume the manifest.
type inventoryGateFactory struct {
	base    *localFactory
	reached chan struct{}
	calls   atomic.Int32
}
type inventoryGateStore struct {
	transferstore.Store
	gate *inventoryGateFactory
}

func (f *inventoryGateFactory) OpenEndpoint(ctx context.Context, e execution.Endpoint) (transferstore.Store, error) {
	s, err := f.base.OpenEndpoint(ctx, e)
	if err != nil {
		return nil, err
	}
	return &inventoryGateStore{Store: s, gate: f}, nil
}
func (s *inventoryGateStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	if s.gate.calls.Add(1) == 129 {
		close(s.gate.reached)
		<-ctx.Done()
		return transferstore.Object{}, ctx.Err()
	}
	return s.Store.Stat(ctx, key)
}
func TestInventoryStopRestartKeepsExactDurableKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	base := &localFactory{}
	f := &inventoryGateFactory{base: base, reached: make(chan struct{})}
	scanner, db := newScanner(t, path, f, Options{})
	root := sourceFiles(t, 131)
	keys := []string{}
	for i := range 130 {
		keys = append(keys, fmt.Sprintf("%04d.txt", i))
	}
	d := inventory.Descriptor{ID: inventorySHA("resume"), Name: "resume.incr", ContentSHA256: inventorySHA(strings.Join(keys, "\n") + "\n"), Binding: inventorySHA("binding"), SpecSHA256: inventorySHA("spec"), ParentID: 1}
	repo := inventory.New(db)
	if _, err := repo.Begin(ctx, d); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(keys); offset += 128 {
		if _, err := repo.Put(ctx, inventory.Batch{ID: d.ID, Number: offset/128 + 1, Keys: keys[offset:min(offset+128, len(keys))]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Seal(ctx, inventory.Seal{ID: d.ID, BatchCount: 2, UniqueCount: 130}); err != nil {
		t.Fatal(err)
	}
	spec := localSpec(root)
	spec.InventoryID = d.ID
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitReached(t, f.reached)
	if _, err := scanner.Stop(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, scanner, Stopped)
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	scanner, db = newScanner(t, path, base, Options{})
	stopped, err := scanner.Status(ctx, 1)
	if err != nil || stopped.Status != Stopped || stopped.TotalObjects != 128 {
		t.Fatal(stopped, err)
	}
	if _, err = scanner.Resume(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if done.TotalObjects != 130 || base.scans.Load() != 0 {
		t.Fatal("resumed scan enlarged or duplicated inventory", done)
	}
	assertBatches(t, scanner, 2, 130)
	// Execute a selected object plus a missing key against real local storage.
	target := t.TempDir()
	jobs, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory := execution.StoreFactoryFunc(func(_ context.Context, j execution.Job) (transferstore.Store, transferstore.Store, error) {
		src, e := transferstore.NewLocal(root)
		if e != nil {
			return nil, nil, e
		}
		dst, e := transferstore.NewLocal(target)
		if e != nil {
			src.Close()
			return nil, nil, e
		}
		return src, dst, nil
	})
	engine, err := execution.NewEngine(jobs, factory, execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	src, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	object, err := src.Stat(ctx, "0000.txt")
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	job := execution.Job{ParentID: 1, ChildID: "1-1", Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: []execution.Item{{ID: "0000.txt", Source: object, DestinationKey: "0000.txt"}, {ID: "missing", Source: transferstore.Object{Key: "missing"}, DestinationKey: "missing"}}}
	if _, err = engine.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(ctx, "1-1", 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, e := engine.Get(ctx, "1-1")
		if e != nil {
			t.Fatal(e)
		}
		if record.FinishedAt != nil {
			if record.Counters.Succeeded != 1 || record.Counters.Failed != 1 {
				t.Fatal(record)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("execution did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 || entries[0].Name() != "0000.txt" {
		t.Fatal("unexpected destination objects", entries, err)
	}
}
