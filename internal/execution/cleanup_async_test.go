package execution

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

type cleanupHeldReader struct {
	io.ReadCloser
	ctx     context.Context
	release <-chan struct{}
}

func (r *cleanupHeldReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r *cleanupHeldReader) Close() error             { <-r.release; return r.ReadCloser.Close() }

func waitExecutionCleanup(t *testing.T, e *Engine, id int64, generation uint64) DeletionProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		progress, err := e.DeletionProgress(ctx, id, generation)
		if err != nil {
			t.Fatal(err)
		}
		if progress.Error != "" {
			t.Fatal(progress.Error)
		}
		if progress.Done {
			return progress
		}
		select {
		case <-ctx.Done():
			t.Fatal("background execution cleanup did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestBackgroundDeleteAdmitsBeforeIOClosesAndFinishesWithoutAnotherRequest(t *testing.T) {
	f := newFixture(t, 2)
	opened, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.startEngine(nil, Options{WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		close(opened)
		return &cleanupHeldReader{ReadCloser: input, ctx: ctx, release: release}, nil
	}})
	f.acceptStart()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("no source opened")
	}
	admitted := make(chan error, 1)
	go func() { admitted <- f.engine.RequestDeleteParent(bg, 224, 1) }()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission waited for live I/O")
	}
	p, err := f.engine.DeletionProgress(bg, 224, 1)
	if err != nil || p.Done || ledgerPayload(t, f.db, 224) == 0 {
		t.Fatalf("cleaned active I/O prematurely: %+v %v", p, err)
	}
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrDeleted) {
		t.Fatalf("accepted after fence: %v", err)
	}
	once.Do(func() { close(release) })
	p = waitExecutionCleanup(t, f.engine, 224, 1)
	if p.RecordsDeleted != 1 || ledgerPayload(t, f.db, 224) != 0 {
		t.Fatalf("wrong committed cleanup: %+v", p)
	}
	if pending, _, err := f.repo.Unfinished(); err != nil || pending != 0 {
		t.Fatalf("pending capacity leaked: %d %v", pending, err)
	}
}

type pauseCleanupCommit struct {
	db        *state.Store
	committed chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (p *pauseCleanupCommit) View(fn func(*state.Tx) error) error { return p.db.View(fn) }
func (p *pauseCleanupCommit) Update(fn func(*state.Tx) error) error {
	err := p.db.Update(fn)
	if err == nil {
		p.once.Do(func() { close(p.committed); <-p.release })
	}
	return err
}

func TestBackgroundDeleteResumesCommittedCursorAfterEngineRestart(t *testing.T) {
	f := newFixture(t, 1)
	const total = 4099
	seedHistory(t, f, total, Completed)
	if err := f.repo.fenceParent(bg, 224, 1, true); err != nil {
		t.Fatal(err)
	}
	held := &pauseCleanupCommit{db: f.db, committed: make(chan struct{}), release: make(chan struct{})}
	f.repo.db = held
	f.startEngine(nil, Options{})
	select {
	case <-held.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("background cleanup never committed")
	}
	p, err := f.repo.Parent(bg, 224)
	if err != nil || p.CleanupAfter == "" || p.CleanupDeleted < 1 || p.CleanupDeleted > int64(state.DefaultCleanupBudget().MaxRecords) || p.CleanupDone {
		t.Fatalf("unbounded or missing checkpoint: %+v %v", p, err)
	}
	short, cancel := context.WithTimeout(bg, 10*time.Millisecond)
	if err := f.engine.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected waiting on active cleanup: %v", err)
	}
	cancel()
	close(held.release)
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	f.engine = nil
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
	f.startEngine(nil, Options{})
	complete := waitExecutionCleanup(t, f.engine, 224, 1)
	if complete.RecordsDeleted != total {
		t.Fatalf("lost progress on restart: %+v", complete)
	}
	if _, err := f.repo.Get(bg, "224-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history remained: %v", err)
	}
	if err := f.engine.RequestDeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	replay, err := f.engine.DeletionProgress(bg, 224, 1)
	if err != nil || replay != complete {
		t.Fatalf("replay reset cleanup: %+v %v", replay, err)
	}
}

func TestDeletedLedgerNeedsOnlyStatusNotFullObjectPayload(t *testing.T) {
	f := newFixture(t, 1)
	other := f.job
	other.ParentID, other.ChildID = 225, "225-1"
	if _, err := f.repo.Accept(bg, other); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(jobsBucket, "00000000000000000224/00000000000000000001", map[string]any{"status": Completed, "job": "old object payload need not decode", "results": "not allocated"}); err != nil {
			return err
		}
		return tx.Put(parentsBucket, "224", ParentFence{ParentID: 224, Generation: 1})
	}); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	if err := f.engine.RequestDeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if p := waitExecutionCleanup(t, f.engine, 224, 1); p.RecordsDeleted != 1 {
		t.Fatalf("wrong cleanup: %+v", p)
	}
	if pending, _, err := f.repo.Unfinished(); err != nil || pending != 1 {
		t.Fatalf("other parent's pending capacity changed: %d %v", pending, err)
	}
}

func TestCompletedCleanupUsesKeysWithoutDecodingColdLedger(t *testing.T) {
	f := newFixture(t, 1)
	if err := f.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(jobsBucket, "00000000000000000224/00000000000000000001", "cold data need not decode as a record"); err != nil {
			return err
		}
		return tx.Put(parentsBucket, "224", ParentFence{ParentID: 224, Generation: 1})
	}); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	if err := f.engine.RequestDeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if p := waitExecutionCleanup(t, f.engine, 224, 1); p.RecordsDeleted != 1 {
		t.Fatalf("wrong cleanup: %+v", p)
	}
}
