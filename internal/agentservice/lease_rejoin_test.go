package agentservice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestRejoinFencesOldQueueAndScannerAcrossRestart(t *testing.T) {
	db, leases, _ := leaseFixture(t)
	ctx := context.Background()
	req := leaseRequest(7, "old", 1)
	req.TTLMillis = 60000
	grantPrepared(t, leases, req)
	source, target := t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte("rejoined transfer\n"), 1024)
	if err := os.WriteFile(filepath.Join(source, "file"), data, 0600); err != nil {
		t.Fatal(err)
	}
	src, err := transferstore.NewLocal(source)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := transferstore.NewLocal(target)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := src.Stat(ctx, "file")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return src, dst, nil
	}), execution.Options{JobContext: leases.JobContext})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)
	old := execution.Job{ParentID: 7, ChildID: "7-1", Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "test"}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: []execution.Item{{ID: "file", Source: obj, DestinationKey: "file"}}}
	if _, err = engine.AcceptSeed(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	leases.quiesce = engine.QuiesceParent
	leases.rejoinCleanup = repo.RetireOwnership
	if err = leases.Rejoin(ctx, LeaseRejoinRequest{ParentID: 7, Generation: 1, OwnerEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := repo.Unfinished(); err != nil || n != 0 {
		t.Fatal("stale ownership still consumes capacity", n, err)
	}
	if row, err := repo.Get(ctx, old.ChildID); err != nil || row.Status != execution.Retired || len(row.Results) != 1 {
		t.Fatal("retirement discarded history", row, err)
	}
	req = leaseRequest(7, "new", 1)
	req.OwnerEpoch = 1
	req.TTLMillis = 60000
	grantPrepared(t, leases, req)
	if err = leases.Retire(ctx, 7, 0); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("delayed old retirement accepted", err)
	}
	if _, err = leases.Prepare(ctx, leaseRequest(7, "delayed", 2)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("delayed old lease accepted", err)
	}
	if _, err = engine.AcceptSeed(ctx, old, nil); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("stale assignment admitted", err)
	}
	if err = engine.Resume(ctx, old.ChildID, 1, true); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("stale queue resumed", err)
	}
	if _, err = leases.SpecContext(discovery.Spec{ParentID: 7, Generation: 1}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("old scanner accepted", err)
	}
	job := old
	job.ChildID = "7-2"
	job.Number = 2
	job.OwnerEpoch = 1
	if _, err = engine.AcceptSeed(ctx, job, nil); err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(ctx, job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row, err := engine.Wait(wait, job.ChildID)
	if err != nil || row.Status != execution.Completed {
		t.Fatal("fresh epoch did not transfer", row, err)
	}
	got, err := os.ReadFile(filepath.Join(target, "file"))
	if err != nil || !bytes.Equal(data, got) {
		t.Fatal("transferred bytes changed", err)
	}
	before, err := leases.JobContext(job)
	if err != nil {
		t.Fatal(err)
	}
	if err = leases.Rejoin(ctx, LeaseRejoinRequest{ParentID: 7, Generation: 1, OwnerEpoch: 1}); err != nil || before.Err() != nil {
		t.Fatal("replayed acknowledgement revoked fresh work", err)
	}
	leases.Close()
	restarted, err := NewLeases(db)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.JobContext(job); err == nil {
		t.Fatal("restart restored a live lease")
	}
	req.LeaseID = "restart"
	grantPrepared(t, restarted, req)
	if _, err = restarted.JobContext(old); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("restart forgot old ownership fence", err)
	}
}

func TestRejoinRetriesDurableQuiescenceAfterFailure(t *testing.T) {
	db, leases, _ := leaseFixture(t)
	ctx := context.Background()
	grantPrepared(t, leases, leaseRequest(1, "initial", 1))
	injected := errors.New("still shutting down")
	leases.quiesce = func(context.Context, int64) error { return injected }
	req := LeaseRejoinRequest{ParentID: 1, Generation: 1, OwnerEpoch: 1}
	if err := leases.Rejoin(ctx, req); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	leases.Close()
	fresh, err := NewLeases(db)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	prepare := leaseRequest(1, "new", 1)
	prepare.OwnerEpoch = 1
	if _, err = fresh.Prepare(ctx, prepare); !errors.Is(err, ErrLeaseRetired) {
		t.Fatal("partial handshake admitted work", err)
	}
	quiesced := false
	fresh.quiesce = func(context.Context, int64) error { quiesced = true; return nil }
	if err = fresh.Rejoin(ctx, req); err != nil || !quiesced {
		t.Fatal("did not finish persisted handshake", err)
	}
	grantPrepared(t, fresh, prepare)
}

func TestRejoinedScannerCanTakeOverLaterWithoutRevivingOldScan(t *testing.T) {
	db, leases, _ := leaseFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := leaseRequest(1, "original", 1)
	req.TTLMillis = 60000
	grantPrepared(t, leases, req)
	scanner, err := discovery.New(db, batchLocalFactory{}, discovery.Options{SpecContext: leases.SpecContext})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	root := t.TempDir()
	if err = os.WriteFile(filepath.Join(root, "file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	original := discovery.Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}
	if _, err = scanner.Start(ctx, original); err != nil {
		t.Fatal(err)
	}
	completed := func(epoch uint64) {
		t.Helper()
		for {
			row, err := scanner.Status(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if row.ScanComplete {
				if row.Epoch != epoch || row.TotalObjects != 1 || row.TotalBytes != 4 {
					t.Fatal("scan lost or duplicated source", row)
				}
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	completed(0)
	leases.quiesce = scanner.QuiesceParent
	leases.rejoinScanner = func(ctx context.Context, req LeaseRejoinRequest) error {
		return scanner.AdoptCompletedOwner(ctx, req.ParentID, req.Generation, *req.ScannerEpoch, req.ScannerThrough, req.OwnerEpoch)
	}
	if err = os.Remove(filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	scanEpoch := uint64(0)
	if err = leases.Rejoin(ctx, LeaseRejoinRequest{ParentID: 1, Generation: 1, OwnerEpoch: 1, ScannerEpoch: &scanEpoch, ScannerThrough: 1}); err != nil {
		t.Fatal(err)
	}
	req.OwnerEpoch, req.LeaseID = 1, "returned"
	grantPrepared(t, leases, req)
	if _, err = scanner.Start(ctx, original); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("old scan restarted after node returned", err)
	}
	replacement := original
	replacement.OwnerEpoch = 1
	if row, err := scanner.Start(ctx, replacement); err != nil || !row.ScanComplete || row.TotalObjects != 1 || row.TotalBytes != 4 {
		t.Fatal("completed adoption rescanned or lost manifest", row, err)
	}
	if err = os.WriteFile(filepath.Join(root, "file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	takeover := discovery.TakeoverRequest{Spec: replacement, Epoch: 2}
	for {
		row, err := scanner.SeedTakeover(ctx, takeover)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Takeover.Cleaning {
			break
		}
	}
	if _, err = scanner.FinalizeTakeover(ctx, 1, 1, 2); err != nil {
		t.Fatal(err)
	}
	completed(2)
	if _, err = scanner.Stop(ctx, 1, 1, 0); !errors.Is(err, discovery.ErrEpoch) {
		t.Fatal("old scanner stop changed replacement", err)
	}
	if err = leases.Retire(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err = leases.Rejoin(ctx, LeaseRejoinRequest{ParentID: 1, Generation: 1, OwnerEpoch: 2}); err != nil {
		t.Fatal(err)
	}
	req.OwnerEpoch, req.LeaseID = 2, "returned-again"
	grantPrepared(t, leases, req)
	for _, oldEpoch := range []uint64{0, 1} {
		if err = leases.Retire(ctx, 1, oldEpoch); !errors.Is(err, ErrLeaseConflict) {
			t.Fatal("older retirement revoked second rejoin", oldEpoch, err)
		}
	}
	if _, err = leases.SpecContext(replacement); !errors.Is(err, ErrLeaseConflict) {
		t.Fatal("previous scanner owner survived second rejoin", err)
	}
}
