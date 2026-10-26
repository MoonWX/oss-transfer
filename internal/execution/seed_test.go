package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type seedCountingStore struct {
	transferstore.Store
	writes, heads *atomic.Int64
}

func (s seedCountingStore) Write(ctx context.Context, key string, input io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.writes.Add(1)
	return s.Store.Write(ctx, key, input, opts)
}
func (s seedCountingStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	s.heads.Add(1)
	return s.Store.Stat(ctx, key)
}

func TestTakeoverSeedRetainsCreditAndOnlyRetriesUnfinishedObjects(t *testing.T) {
	f := newFixture(t, 3)
	f.startEngine(nil, Options{})
	f.acceptStart()
	seed := f.wait()
	if seed.Status != Completed {
		t.Fatalf("initial transfer: %+v", seed)
	}
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	oldJob := f.job
	f.job.Assignment = 1
	seed.Job = f.job
	seed.Fingerprint = "fingerprint belongs to the old mount and must not be trusted"
	seed.Results[1].Status = ItemSkipped
	seed.Results[2].Status = ItemFailed
	seed.Results[2].Error = "previous upload failed"
	if err := os.Remove(filepath.Join(f.dst, f.job.Items[2].DestinationKey)); err != nil {
		t.Fatal(err)
	}
	writes, heads := &atomic.Int64{}, &atomic.Int64{}
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, j)
		return src, seedCountingStore{dst, writes, heads}, err
	}), Options{})
	accepted, err := f.engine.AcceptSeed(bg, f.job, &seed)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != Recoverable || accepted.Counters.Succeeded != 1 || accepted.Counters.Skipped != 1 || accepted.Counters.Failed != 0 {
		t.Fatalf("seed credit reset: %+v", accepted)
	}
	if _, err := f.engine.Accept(bg, oldJob); !errors.Is(err, ErrAssignment) {
		t.Fatalf("stale owner accepted: %v", err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	result := f.wait()
	if result.Status != Completed || result.Counters.Succeeded != 2 || result.Counters.Skipped != 1 {
		t.Fatalf("recovery result: %+v", result)
	}
	if writes.Load() != 1 || heads.Load() < 2 {
		t.Fatalf("want one upload and target checks for retained credits; writes=%d heads=%d", writes.Load(), heads.Load())
	}
	again, err := f.engine.AcceptSeed(bg, f.job, &seed)
	if err != nil || again.Status != Completed || again.Counters != result.Counters {
		t.Fatalf("repeated seed reset newer progress: %+v %v", again, err)
	}
	f.assertFiles()
}
func TestSharedDestinationTakeoverUsesPortableFingerprint(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	f.acceptStart()
	seed := f.wait()
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.job.Assignment = 1
	f.job.SharedLocalDestination = true
	seed.Job = f.job
	for i := range seed.Results {
		seed.Results[i].Target.ETag = "local:old-device:old-inode"
	}
	writes, heads := &atomic.Int64{}, &atomic.Int64{}
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, j)
		return src, seedCountingStore{dst, writes, heads}, err
	}), Options{})
	if _, err := f.engine.AcceptSeed(bg, f.job, &seed); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.TryRecover(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	result := f.wait()
	if result.Status != Completed || writes.Load() != 0 || heads.Load() != 2 {
		t.Fatalf("shared-mount identity caused retransfers: %+v writes=%d heads=%d", result, writes.Load(), heads.Load())
	}
}
