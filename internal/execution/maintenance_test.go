package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func ledgerPayload(t *testing.T, db *state.Store, parentID int64) int64 {
	t.Helper()
	var size int64
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(jobsBucket, fmt.Sprintf("%020d/", parentID), "", 4096, func(_ string, raw json.RawMessage) error {
			size += int64(len(raw))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return size
}

func TestPermanentDeleteReclaimsLedgerButRetainsFenceAndTransferredFiles(t *testing.T) {
	f := newFixture(t, 4)
	f.startEngine(nil, Options{})
	f.acceptStart()
	if f.wait().Status != Completed {
		t.Fatal("copy did not complete")
	}
	other := f.job
	other.ParentID, other.ChildID = 225, "225-1"
	if _, err := f.engine.Accept(bg, other); err != nil {
		t.Fatal(err)
	}
	if size := ledgerPayload(t, f.db, 224); size == 0 {
		t.Fatal("fixture has no ledger")
	}
	if err := f.engine.DeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if size := ledgerPayload(t, f.db, 224); size != 0 {
		t.Fatalf("permanent delete retained %d bytes of object history", size)
	}
	f.assertFiles()
	if _, err := f.repo.Get(bg, other.ChildID); err != nil {
		t.Fatalf("deleted another parent's task: %v", err)
	}
	if pending, _, err := f.repo.Unfinished(); err != nil || pending != 1 {
		t.Fatalf("wrong remaining admission count: %d %v", pending, err)
	}
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
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrDeleted) {
		t.Fatalf("restarted Agent accepted a deleted task: %v", err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); !errors.Is(err, ErrDeleted) {
		t.Fatalf("late resume resurrected deleted work: %v", err)
	}
	if err := f.engine.DeleteParent(bg, 224, 1); err != nil {
		t.Fatalf("lost delete response could not be retried: %v", err)
	}
}

func TestPermanentDeleteWaitsForSourceCloseBeforeReclaiming(t *testing.T) {
	f := newFixture(t, 2)
	ready := make(chan struct{})
	var closed atomic.Int64
	f.startEngine(nil, Options{WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		close(ready)
		return &cancelReader{input: input, ctx: ctx, closed: &closed}, nil
	}})
	f.acceptStart()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("source never opened")
	}
	if err := f.engine.DeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 || ledgerPayload(t, f.db, 224) != 0 {
		t.Fatal("deletion did not drain I/O and reclaim the ledger")
	}
	if pending, _, err := f.repo.Unfinished(); err != nil || pending != 0 {
		t.Fatalf("running deletion retained admission capacity: %d %v", pending, err)
	}
}

// Abort after a successful page commit, as a process dying between cleanup
// transactions would do. This also bounds how much any one transaction removes.
type cancelAfterUpdate struct {
	db     *state.Store
	cancel context.CancelFunc
	count  int
}

func (d *cancelAfterUpdate) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *cancelAfterUpdate) Update(fn func(*state.Tx) error) error {
	err := d.db.Update(fn)
	if err == nil {
		d.count++
		if d.count == 1 {
			d.cancel()
		}
	}
	return err
}

func seedHistory(t *testing.T, f *fixture, count int, status Status) {
	t.Helper()
	if err := f.db.Update(func(tx *state.Tx) error {
		for n := 1; n <= count; n++ {
			job := f.job
			job.Number, job.ChildID = n, fmt.Sprintf("224-%d", n)
			record := Record{Job: job, Status: status, Error: strings.Repeat("retained history ", 256)}
			key, _ := jobKey(job.ChildID)
			if err := tx.Put(jobsBucket, key, record); err != nil {
				return err
			}
		}
		return tx.Put(parentsBucket, "224", ParentFence{ParentID: 224, Generation: 1, Stopped: status == Deleted || status == Stopped, Deleted: status == Deleted})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalDeletedCleanupResumesAfterCommittedPage(t *testing.T) {
	f := newFixture(t, 1)
	seedHistory(t, f, 300, Deleted)
	// This is an old database that never had a running index or purge queue.
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Delete(metaBucket, "running-index-v1") }); err != nil {
		t.Fatal(err)
	}
	var err error
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := ledgerPayload(t, f.db, 224)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	interrupted := &cancelAfterUpdate{db: f.db, cancel: cancel}
	f.repo.db = interrupted
	if err := f.repo.Recover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup did not respect interruption: %v", err)
	}
	page, err := f.repo.List(bg, 224, "", 256)
	if err != nil || len(page) != 300-maintenancePageSize {
		t.Fatalf("cleanup transaction was not bounded: %d %v", len(page), err)
	}
	if remaining := ledgerPayload(t, f.db, 224); remaining == 0 || remaining >= before {
		t.Fatalf("partial cleanup did not reclaim committed pages: before=%d after=%d", before, remaining)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Recover(bg); err != nil {
		t.Fatal(err)
	}
	if remaining := ledgerPayload(t, f.db, 224); remaining != 0 {
		t.Fatalf("restart left %d bytes of deleted object history", remaining)
	}
	t.Logf("permanently deleted object ledger: %d logical bytes -> 0; parent deletion fence retained", before)
	if _, err := f.repo.Accept(bg, f.job); !errors.Is(err, ErrDeleted) {
		t.Fatalf("cleanup lost deletion fence: %v", err)
	}
}

func TestRunningIndexBackfillAndRecoveryPreserveStoppedHistory(t *testing.T) {
	f := newFixture(t, 1)
	seedHistory(t, f, 300, Stopped)
	key, _ := jobKey("224-257")
	if err := f.db.Update(func(tx *state.Tx) error {
		var rec Record
		if _, err := tx.Get(jobsBucket, key, &rec); err != nil {
			return err
		}
		rec.Status = Running
		if err := tx.Put(jobsBucket, key, rec); err != nil {
			return err
		}
		return tx.Delete(metaBucket, "running-index-v1")
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	running, err := f.repo.running(bg, "", 128)
	if err != nil || len(running) != 1 || running[0].Job.ChildID != "224-257" {
		t.Fatalf("backfill missed running child after first page: %+v %v", running, err)
	}
	before := ledgerPayload(t, f.db, 224)
	if err := f.repo.Recover(bg); err != nil {
		t.Fatal(err)
	}
	if after := ledgerPayload(t, f.db, 224); after < before/2 {
		t.Fatal("startup cleanup removed stopped/recycled task history")
	}
	if running, err := f.repo.running(bg, "", 128); err != nil || len(running) != 0 {
		t.Fatalf("recovered child remained in running index: %d %v", len(running), err)
	}
	if rec, err := f.repo.Get(bg, "224-257"); err != nil || rec.Status != Stopped {
		t.Fatalf("recovery ignored stopped parent: %s %v", rec.Status, err)
	}
}

func TestRunningIndexBuildResumesFromDurableCursor(t *testing.T) {
	f := newFixture(t, 1)
	seedHistory(t, f, 300, Completed)
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Delete(metaBucket, "running-index-v1") }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	f.repo.db = &cancelAfterUpdate{db: f.db, cancel: cancel}
	if err := f.repo.ensureRunningIndex(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("index build did not stop between pages: %v", err)
	}
	var index runningIndexState
	if err := f.db.View(func(tx *state.Tx) error { _, err := tx.Get(metaBucket, "running-index-v1", &index); return err }); err != nil {
		t.Fatal(err)
	}
	want, _ := jobKey("224-128")
	if index.Complete || index.After != want {
		t.Fatalf("backfill did not save its committed cursor: %+v", index)
	}
	// Prove the second build does not decode the committed prefix again.
	if err := f.db.Update(func(tx *state.Tx) error {
		key, _ := jobKey("224-1")
		return tx.Put(jobsBucket, key, "already visited")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRepository(f.db, RepositoryOptions{}); err != nil {
		t.Fatalf("resumed build rescanned committed pages: %v", err)
	}
}

func TestCompletedHistoryIsNotDecodedDuringSubsequentStartup(t *testing.T) {
	f := newFixture(t, 1)
	// This deliberately decodable-as-JSON but invalid-as-Record value stands
	// for cold history. Startup must not touch it once the index is built.
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put(jobsBucket, "00000000000000000225/00000000000000000001", "cold history must not be decoded")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.begin(bg, f.job.ChildID, 1, false, false); err != nil {
		t.Fatal(err)
	}
	var err error
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Recover(bg); err != nil {
		t.Fatalf("startup decoded completed history instead of the running index: %v", err)
	}
	if rec, err := f.repo.Get(bg, f.job.ChildID); err != nil || rec.Status != Recoverable {
		t.Fatalf("startup failed to recover indexed running job: %s %v", rec.Status, err)
	}
}
