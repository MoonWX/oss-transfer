package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
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
			m, err := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
				if opens.Add(1) == 1 {
					return source, dest, nil
				}
				return generatedStore{}, generatedStore{}, nil
			}), nil, Options{})
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
				if err != nil || p.Status != "RUNNING" {
					t.Fatalf("published a terminal status while resources are open: %+v %v", p.Progress, err)
				}
				if err := m.Delete("test"); !errors.Is(err, ErrBusy) {
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
		})
	}
}

func TestManagerCapsStoredDifferencesKeepsFullCountsAndPrunesOldRuns(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := LegacyStoredDifferences + 3
	m, err := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return generatedStore{count: count}, generatedStore{}, nil
	}), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	finish := func(id string) Page {
		t.Helper()
		r := request()
		r.ID = id
		// The previous run's worker has exited, so its slot must be free.
		if _, err = m.Start(r); err != nil {
			t.Fatal(err)
		}
		// This checks durable retention of 10,003 differences, not disk speed.
		waitWorkers(t, m)
		p, e := m.Get(id, 0, 50)
		if e != nil || p.Status != "COMPLETED" {
			t.Fatalf("comparison did not complete: %+v %v", p.Progress, e)
		}
		return p
	}
	p := finish("oldest")
	if p.Rows != LegacyStoredDifferences || !p.RowsTruncated || p.Missing != int64(count) || p.Compared != int64(count) {
		t.Fatalf("cap lost totals: %+v", p.Progress)
	}
	count = 0
	for i := 0; i < legacyRetainedComparisons; i++ {
		finish(fmt.Sprintf("later-%02d", i))
	}
	if _, err = m.Get("oldest", 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest result not pruned: %v", err)
	}
	var leftover int
	_ = db.View(func(tx *state.Tx) error {
		return tx.Scan("comparison_rows", "oldest/", "", 1, func(string, json.RawMessage) error { leftover++; return nil })
	})
	if leftover != 0 {
		t.Fatal("pruning left orphaned difference rows")
	}
}

type blockedStore struct{ transferstore.Store }

func (s blockedStore) List(ctx context.Context, _ transferstore.ListOptions) (transferstore.Page, error) {
	<-ctx.Done()
	return transferstore.Page{}, ctx.Err()
}

func TestManagerCancellationPersistenceAndSingleActiveBound(t *testing.T) {
	db, e := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	a, b := local(t), local(t)
	factory := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return blockedStore{a}, b, nil
	})
	m, e := New(db, factory, nil, Options{})
	if e != nil {
		t.Fatal(e)
	}
	r := request()
	if _, e = m.Start(r); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	if _, e = m.Start(r); !errors.Is(e, ErrBusy) {
		t.Fatalf("unbounded active comparisons: %v", e)
	}
	if _, e = m.Cancel("test"); e != nil {
		t.Fatal(e)
	}
	waitWorkers(t, m)
	if p, e := m.Get("test", 0, 50); e != nil || p.Status != "CANCELED" {
		t.Fatalf("cancel did not finish: %+v %v", p.Progress, e)
	}
	m.Close()
	m, e = New(db, factory, nil, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	p, e := m.Get("test", 0, 50)
	if e != nil || p.Status != "CANCELED" {
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
	}), nil, Options{})
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
