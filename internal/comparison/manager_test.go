package comparison

import (
	"context"
	"errors"
	"fmt"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type generatedStore struct {
	transferstore.Store
	count int
}

func (s generatedStore) List(_ context.Context, options transferstore.ListOptions) (transferstore.Page, error) {
	start, _ := strconv.Atoi(options.Cursor)
	end := min(start+options.Limit, s.count)
	p := transferstore.Page{}
	for i := start; i < end; i++ {
		p.Objects = append(p.Objects, transferstore.Object{Key: fmt.Sprintf("src/%08d", i), Size: 1, ETag: "stable"})
	}
	if end < s.count {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}

func (s generatedStore) Stat(_ context.Context, key string) (transferstore.Object, error) {
	i, e := strconv.Atoi(strings.TrimPrefix(key, "src/"))
	if e != nil || !strings.HasPrefix(key, "src/") || i < 0 || i >= s.count {
		return transferstore.Object{}, transferstore.ErrNotFound
	}
	return transferstore.Object{Key: key, Size: 1, ETag: "stable"}, nil
}

func (s generatedStore) Close() error { return nil }

type heldCloseStore struct {
	generatedStore
	entered  chan struct{}
	release  chan struct{}
	closed   atomic.Bool
	once     sync.Once
	closeErr error
}

func (s *heldCloseStore) Close() error {
	close(s.entered)
	<-s.release
	s.closed.Store(true)
	return s.closeErr
}

func (s *heldCloseStore) unblock() { s.once.Do(func() { close(s.release) }) }

func TestTerminalStatusWaitsForStoresAndReleasesActiveSlot(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "canceled", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			source := &heldCloseStore{entered: make(chan struct{}), release: make(chan struct{})}
			dest := &heldCloseStore{entered: make(chan struct{}), release: make(chan struct{})}
			if mode == "failed" {
				source.closeErr = errors.New("storage close failed")
			}
			var opens atomic.Int32
			log := &testLog{}
			m, err := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
				if opens.Add(1) == 1 {
					return source, dest, nil
				}
				return generatedStore{}, generatedStore{}, nil
			}), nil, Options{Logf: log.logf})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { dest.unblock(); source.unblock(); m.Close() }()
			if _, err = m.Start(request()); err != nil {
				t.Fatal(err)
			}
			wait := func(done <-chan struct{}) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("resource close did not reach its gate")
				}
			}
			wait(dest.entered)
			next := request()
			next.ID = "next"
			assertClosing := func() {
				t.Helper()
				p, err := m.Get("test", 0, 1)
				// CANCELING is committed by Cancel before the run stops; it is
				// not a terminal state.
				if err != nil || p.Status != StateRunning && p.Status != StateCanceling {
					t.Fatalf("published a terminal status while resources are open: %+v %v", p.Progress, err)
				}
				if err := m.Delete("test"); !errors.Is(err, ErrRunning) {
					t.Fatalf("deleted a comparison while its resources are open: %v", err)
				}
			}
			assertClosing()
			if _, err := m.Start(next); !errors.Is(err, ErrBusy) {
				t.Fatalf("admitted another comparison before resources closed: %v", err)
			}
			want := "COMPLETED"
			var stopped chan struct{}
			switch mode {
			case "failed":
				want = "FAILED"
			case "canceled":
				want = "CANCELED"
				if p, err := m.Cancel("test"); err != nil || p.Status != "CANCELING" {
					t.Fatal(p, err)
				}
			case "shutdown":
				want = "INTERRUPTED"
				stopped = make(chan struct{})
				go func() { m.Close(); close(stopped) }()
				wait(m.ctx.Done())
				select {
				case <-stopped:
					t.Fatal("Manager.Close returned before storage resources closed")
				default:
				}
			}
			dest.unblock()
			wait(source.entered)
			assertClosing()
			source.unblock()
			deadline := time.Now().Add(2 * time.Minute) // hang guard only
			for {
				p, err := m.Get("test", 0, 1)
				if err != nil {
					t.Fatal(err)
				}
				if p.Status == want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("terminal status missing: %+v", p.Progress)
				}
				time.Sleep(time.Millisecond)
			}
			if !source.closed.Load() || !dest.closed.Load() {
				t.Fatal("terminal status preceded resource closure")
			}
			// No retry: observing a terminal state must make deletion and the
			// next admission immediately valid, not depend on another defer.
			if err := m.Delete("test"); err != nil {
				t.Fatalf("terminal comparison retained its active slot: %v", err)
			}
			_, err = m.Start(next)
			if mode == "shutdown" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("shutdown admitted another comparison", err)
				}
				wait(stopped)
			} else if err != nil {
				t.Fatal("completed resource cleanup did not release admission", err)
			}
			if mode == "failed" && !log.contains("comparison test failed: ") {
				t.Fatalf("failure was not logged: %q", log.lines)
			}
		})
	}
}

// Agents before Protocol 2 kept 10,000 rows and 10 comparisons. Every
// difference is now stored and no comparison is pruned for being the 11th.
func TestManagerStoresEveryDifferenceAndKeepsEveryComparison(t *testing.T) {
	h := newHarness(t)
	count := LegacyStoredDifferences + 3
	m := h.open(execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return generatedStore{count: count}, generatedStore{}, nil
	}))
	p := startAndWait(t, m, requestWithID("first"))
	if p.Status != StateCompleted || p.Rows != int64(count) || p.RowsTruncated || p.Missing != int64(count) || p.Compared != int64(count) || p.ResultLimit != 0 || p.ResultsProtocol != Protocol {
		t.Fatalf("differences were capped or miscounted: %+v", p)
	}
	count = 1
	for i := 0; i < 12; i++ {
		startAndWait(t, m, requestWithID(fmt.Sprintf("later-%02d", i)))
	}
	m.retain()
	items, err := m.List()
	if err != nil || len(items) != 13 || items[len(items)-1].ID != "first" {
		t.Fatalf("comparisons were pruned: %d %v", len(items), err)
	}
	rows := allRows(t, m, RowsQuery{ID: "first", Limit: MaxPageRows})
	if len(rows) != LegacyStoredDifferences+3 || rows[len(rows)-1].Key != fmt.Sprintf("%08d", LegacyStoredDifferences+2) {
		t.Fatalf("stored rows lost: %d", len(rows))
	}
}

type blockedStore struct{ transferstore.Store }

func (s blockedStore) List(ctx context.Context, _ transferstore.ListOptions) (transferstore.Page, error) {
	<-ctx.Done()
	return transferstore.Page{}, ctx.Err()
}

func TestManagerCancellationPersistenceAndSingleActiveBound(t *testing.T) {
	h := newHarness(t)
	a, b := local(t), local(t)
	factory := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return blockedStore{a}, b, nil
	})
	m := h.open(factory)
	r := request()
	if _, e := m.Start(r); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	if _, e := m.Start(r); !errors.Is(e, ErrBusy) {
		t.Fatalf("unbounded active comparisons: %v", e)
	}
	if _, e := m.Cancel("test"); e != nil {
		t.Fatal(e)
	}
	waitWorkers(t, m)
	if p, e := m.Get("test", 0, 50); e != nil || p.Status != StateCanceled || !p.Resumable || p.EndedAt.IsZero() {
		t.Fatalf("cancel did not finish: %+v %v", p.Progress, e)
	}
	m.Close()
	m = h.open(factory)
	p, e := m.Get("test", 0, 50)
	if e != nil || p.Status != StateCanceled {
		t.Fatalf("state lost after restart: %+v %v", p, e)
	}
}

func TestManagerBatchesOnlyDifferencesAndPagesResults(t *testing.T) {
	db, e := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	a, b := local(t), local(t)
	for i := 0; i < 140; i++ {
		put(t, a, fmt.Sprintf("src/%04d", i), "x")
	}
	put(t, a, "src/match", "same")
	put(t, b, "backup/match", "same")
	m, e := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return a, b, nil
	}), nil, Options{Logf: (&testLog{}).logf})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if _, e = m.Start(request()); e != nil {
		t.Fatal(e)
	}
	waitWorkers(t, m)
	p, e := m.Get("test", 0, 100)
	if e != nil || p.Status != "COMPLETED" {
		t.Fatalf("comparison did not complete: %+v %v", p.Progress, e)
	}
	if p.Compared != 141 || p.Matched != 1 || p.Missing != 140 || p.Rows != 140 || len(p.Items) != 100 || p.NextAfter != 100 {
		t.Fatalf("wrong totals/pagination: %+v", p.Progress)
	}
	p, e = m.Get("test", 100, 100)
	if e != nil || len(p.Items) != 40 || p.NextAfter != 0 || p.Items[0].Number != 101 {
		t.Fatalf("lost persisted rows: %+v %v", p, e)
	}
}

// Without Options.Directory, results live in a private temporary directory
// that Close removes.
func TestManagerTemporaryResultsDirectory(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := New(db, storesFactory(newFakeStore(object("src/a", 1)), newFakeStore()), nil, Options{Logf: (&testLog{}).logf})
	if err != nil {
		t.Fatal(err)
	}
	if p := startAndWait(t, m, request()); p.Rows != 1 {
		t.Fatalf("result not stored: %+v", p)
	}
	dir := m.dir
	if _, err := os.Stat(filepath.Join(dir, resultDirectory("test"), resultsFile)); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary results survived Close: %v", err)
	}
}
