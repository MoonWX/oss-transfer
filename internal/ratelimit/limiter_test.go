package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

var bg = context.Background()

type fixture struct {
	db      *state.Store
	path    string
	limiter *Limiter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, path: path, limiter: l}
	t.Cleanup(func() { f.limiter.Close(); f.db.Close() })
	return f
}
func apply(t *testing.T, l *Limiter, nodeRate, parentRate int64) {
	t.Helper()
	if err := l.ApplyNode(bg, NodeLimit{Revision: 1, BandwidthBytesPerSecond: nodeRate}); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 1, BandwidthBytesPerSecond: parentRate}); err != nil {
		t.Fatal(err)
	}
}

func TestLimitsSurviveDatabaseReopenAndRejectStaleUpdates(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 12500000, 6250000)
	if err := f.limiter.ApplyNode(bg, NodeLimit{Revision: 1, BandwidthBytesPerSecond: 9}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting node replay accepted: %v", err)
	}
	p := ParentLimit{ParentID: 1, Generation: 1, Revision: 2, BandwidthBytesPerSecond: 3000000}
	if err := f.limiter.ApplyParent(bg, p); err != nil {
		t.Fatal(err)
	}
	if err := f.limiter.ApplyParent(bg, p); err != nil {
		t.Fatal(err)
	}
	stale := p
	stale.Revision = 1
	if err := f.limiter.ApplyParent(bg, stale); !errors.Is(err, ErrStale) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	conflict := p
	conflict.BandwidthBytesPerSecond++
	if err := f.limiter.ApplyParent(bg, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting revision accepted: %v", err)
	}
	f.limiter.Close()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.limiter, err = New(f.db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.limiter.Node(); got.BandwidthBytesPerSecond != 12500000 || got.Revision != 1 {
		t.Fatalf("node rate not restored: %+v", got)
	}
	got, err := f.limiter.Parent(1)
	if err != nil || got != p {
		t.Fatalf("parent share not restored: %v %+v", err, got)
	}
	stopped := p
	stopped.Revision = 3
	stopped.Stopped = true
	if err := f.limiter.ApplyParent(bg, stopped); err != nil {
		t.Fatal(err)
	}
	unstop := stopped
	unstop.Revision++
	unstop.Stopped = false
	if err := f.limiter.ApplyParent(bg, unstop); !errors.Is(err, ErrStopped) {
		t.Fatalf("same-generation stop fence bypassed: %v", err)
	}
	unstop.Generation = 2
	if err := f.limiter.ApplyParent(bg, unstop); err != nil {
		t.Fatal(err)
	}
	if err := f.limiter.ApplyParent(bg, p); !errors.Is(err, ErrStale) {
		t.Fatalf("old generation revived: %v", err)
	}
}

type failDB struct {
	db   *state.Store
	fail atomic.Bool
}

func (d *failDB) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *failDB) Update(fn func(*state.Tx) error) error {
	return d.db.Update(func(tx *state.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if d.fail.Load() {
			return errors.New("durability failure")
		}
		return nil
	})
}
func TestPersistenceFailureDoesNotChangeRuntimeCaps(t *testing.T) {
	f := newFixture(t)
	f.limiter.Close()
	db := &failDB{db: f.db}
	var err error
	f.limiter, err = New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.limiter, 1000, 500)
	db.fail.Store(true)
	if err := f.limiter.ApplyNode(bg, NodeLimit{Revision: 2, BandwidthBytesPerSecond: 0}); err == nil {
		t.Fatal("failed node save succeeded")
	}
	if f.limiter.Node().BandwidthBytesPerSecond != 1000 {
		t.Fatal("runtime node cap changed before durable save")
	}
	if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, BandwidthBytesPerSecond: 0}); err == nil {
		t.Fatal("failed parent save succeeded")
	}
	p, _ := f.limiter.Parent(1)
	if p.BandwidthBytesPerSecond != 500 {
		t.Fatal("runtime parent cap changed before durable save")
	}
	f.limiter.Close()
	f.limiter, err = New(f.db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.limiter.Node().BandwidthBytesPerSecond != 1000 {
		t.Fatal("failed transaction leaked disk cap")
	}
}

func TestParallelReadersShareParentAndNodeBandwidth(t *testing.T) {
	for _, tc := range []struct {
		name         string
		node, parent int64
		parents      int
	}{
		{"parent-share", 0, 256 << 10, 1}, {"node-shared-across-parents", 256 << 10, 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			apply(t, f.limiter, tc.node, tc.parent)
			for id := 2; id <= tc.parents; id++ {
				if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: int64(id), Generation: 1, Revision: 1, BandwidthBytesPerSecond: tc.parent}); err != nil {
					t.Fatal(err)
				}
			}
			const readers = 4
			const perReader = 96 << 10
			start := time.Now()
			var wg sync.WaitGroup
			errs := make(chan error, readers)
			for i := range readers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := int64(1)
					if tc.parents > 1 {
						id = int64(i + 1)
					}
					input, err := f.limiter.Wrap(bg, id, 1, io.NopCloser(bytes.NewReader(make([]byte, perReader))), perReader)
					if err != nil {
						errs <- err
						return
					}
					defer input.Close()
					n, err := io.Copy(io.Discard, input)
					if err == nil && n != perReader {
						err = errors.New("short transfer")
					}
					errs <- err
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			elapsed := time.Since(start)
			expected := 1500 * time.Millisecond
			// A per-reader bucket would finish in about 375 ms; a free initial
			// 64 KiB grant would also exceed the aggregate cap by a visible margin.
			if elapsed < expected-35*time.Millisecond || elapsed > 6*time.Second {
				t.Fatalf("aggregate bandwidth incorrect: %v for %d bytes", elapsed, readers*perReader)
			}
		})
	}
}

type readObserver struct {
	io.Reader
	maxRead int
	calls   int
	closed  bool
}

func (r *readObserver) Read(p []byte) (int, error) {
	r.calls++
	r.maxRead = max(r.maxRead, len(p))
	return r.Reader.Read(p)
}
func (r *readObserver) Close() error { r.closed = true; return nil }
func TestNoInitialBurstAndEveryReadIsBounded(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 0, 64<<10)
	// The delay between configuration and a task's first read must not preload
	// a bucket with free transfer bytes.
	time.Sleep(120 * time.Millisecond)
	input := &readObserver{Reader: bytes.NewReader(make([]byte, 128<<10))}
	r, err := f.limiter.Wrap(bg, 1, 1, input, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	start := time.Now()
	n, err := r.Read(make([]byte, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if n > 64<<10 || time.Since(start) < 80*time.Millisecond {
		t.Fatalf("initial read bypassed budget: %d bytes in %v", n, time.Since(start))
	}
	if input.maxRead > 64<<10 {
		t.Fatalf("unbounded source read: %d", input.maxRead)
	}
}

func TestDynamicUnlimitedUpdateWakesBlockedRead(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 0, 1)
	r, err := f.limiter.Wrap(bg, 1, 1, io.NopCloser(bytes.NewReader([]byte("x"))), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := make(chan error, 1)
	go func() { var one [1]byte; _, err := r.Read(one[:]); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("initial 1 byte/s read did not wait: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	start := time.Now()
	if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, BandwidthBytesPerSecond: 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 300*time.Millisecond {
			t.Fatal("rate change did not wake budget wait")
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after unlimited update")
	}
}

type shortHeldInput struct{ entered, release chan struct{} }

func (r *shortHeldInput) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	p[0] = 'x'
	return 1, nil
}
func (r *shortHeldInput) Close() error { return nil }

func TestLowerCapDoesNotReceiveRefundFromOldRevision(t *testing.T) {
	for _, scope := range []string{"node", "parent"} {
		t.Run(scope, func(t *testing.T) {
			f := newFixture(t)
			if scope == "node" {
				apply(t, f.limiter, 1<<20, 0)
			} else {
				apply(t, f.limiter, 0, 1<<20)
			}
			input := &shortHeldInput{entered: make(chan struct{}), release: make(chan struct{})}
			r, err := f.limiter.Wrap(bg, 1, 1, input, 64<<10)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			done := make(chan error, 1)
			go func() { _, err := r.Read(make([]byte, 64<<10)); done <- err }()
			<-input.entered
			if scope == "node" {
				err = f.limiter.ApplyNode(bg, NodeLimit{Revision: 2, BandwidthBytesPerSecond: 100})
			} else {
				err = f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, BandwidthBytesPerSecond: 100})
			}
			if err != nil {
				t.Fatal(err)
			}
			close(input.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// The first reader refunds 65,535 unused bytes from the old cap. None may
			// become immediate credit under the new 100 byte/s budget.
			next, err := f.limiter.Wrap(bg, 1, 1, io.NopCloser(bytes.NewReader(make([]byte, 10))), 10)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			start := time.Now()
			if n, err := next.Read(make([]byte, 10)); err != nil || n != 10 {
				t.Fatalf("read: %d %v", n, err)
			}
			if time.Since(start) < 80*time.Millisecond {
				t.Fatalf("old reservation bypassed lower cap: %v", time.Since(start))
			}
		})
	}
}

func TestStopAndGenerationChangeCancelWaitingReads(t *testing.T) {
	for _, mode := range []string{"stop", "generation", "context", "close"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			apply(t, f.limiter, 1, 1)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			input := &atomicCloser{Reader: bytes.NewReader([]byte("x"))}
			r, err := f.limiter.Wrap(ctx, 1, 1, input, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			done := make(chan error, 1)
			go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
			time.Sleep(20 * time.Millisecond)
			switch mode {
			case "stop":
				err = f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, Stopped: true})
			case "generation":
				err = f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 2, Revision: 1})
			case "context":
				cancel()
			case "close":
				f.limiter.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled budget read succeeded")
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("cancel did not wake blocked reader")
			}
			deadline := time.Now().Add(time.Second)
			for !input.closed.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !input.closed.Load() {
				t.Fatal("underlying reader not closed on cancellation")
			}
		})
	}
}

type atomicCloser struct {
	io.Reader
	closed atomic.Bool
}

func (r *atomicCloser) Close() error { r.closed.Store(true); return nil }

type blockedInput struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (r *blockedInput) Read([]byte) (int, error) {
	close(r.entered)
	<-r.closed
	return 0, io.ErrClosedPipe
}
func (r *blockedInput) Close() error { r.once.Do(func() { close(r.closed) }); return nil }
func TestParentStopInterruptsUnderlyingIOAfterGrant(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 0, 0)
	raw := &blockedInput{entered: make(chan struct{}), closed: make(chan struct{})}
	r, err := f.limiter.Wrap(bg, 1, 1, raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
	<-raw.entered
	if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, Stopped: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("unexpected stopped I/O result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("underlying I/O did not cancel")
	}
}

func TestMissingAllocationAndCapacityFailClosed(t *testing.T) {
	f := newFixture(t)
	f.limiter.Close()
	var err error
	f.limiter, err = New(f.db, Options{MaxParents: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.limiter.Wrap(bg, 1, 1, io.NopCloser(bytes.NewReader(nil)), 0); !errors.Is(err, ErrMissing) {
		t.Fatalf("unallocated transfer was unlimited: %v", err)
	}
	apply(t, f.limiter, 0, 0)
	if _, err := f.limiter.Wrap(bg, 1, 2, io.NopCloser(bytes.NewReader(nil)), 0); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong generation accepted: %v", err)
	}
	if err := f.limiter.ApplyParent(bg, ParentLimit{ParentID: 2, Generation: 1, Revision: 1}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("allocation capacity not enforced: %v", err)
	}
}

func TestRealExecutionSharesConfiguredByteBudget(t *testing.T) {
	f := newFixture(t)
	apply(t, f.limiter, 0, 256<<10)
	base := t.TempDir()
	src, dst := filepath.Join(base, "source"), filepath.Join(base, "target")
	for _, dir := range []string{src, dst} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	job := execution.Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 1, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Source: execution.Endpoint{Kind: "LOCAL", Root: src}, Destination: execution.Endpoint{Kind: "LOCAL", Root: dst}}
	source, err := transferstore.NewLocal(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c", "d"} {
		data := bytes.Repeat([]byte(key), 64<<10)
		if err := os.WriteFile(filepath.Join(src, key), data, 0600); err != nil {
			t.Fatal(err)
		}
		obj, err := source.Stat(bg, key)
		if err != nil {
			t.Fatal(err)
		}
		job.Items = append(job.Items, execution.Item{ID: key, Source: obj, DestinationKey: key})
	}
	source.Close()
	repo, _ := execution.NewRepository(f.db, execution.RepositoryOptions{})
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(ctx context.Context, j execution.Job) (transferstore.Store, transferstore.Store, error) {
		s, err := transferstore.NewLocal(j.Source.Root)
		if err != nil {
			return nil, nil, err
		}
		d, err := transferstore.NewLocal(j.Destination.Root)
		return s, d, err
	}), execution.Options{WorkersPerJob: 4, DecorateReader: f.limiter.Decorator})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(bg)
	if _, err := engine.Accept(bg, job); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := engine.Start(bg, job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	record, err := engine.Wait(ctx, job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != execution.Completed || record.Counters.SucceededBytes != 256<<10 {
		t.Fatalf("real limited transfer failed: %+v", record)
	}
	if elapsed := time.Since(start); elapsed < 970*time.Millisecond {
		t.Fatalf("four workers bypassed parent budget: %v", elapsed)
	}
	for _, item := range job.Items {
		want, _ := os.ReadFile(filepath.Join(src, item.Source.Key))
		got, err := os.ReadFile(filepath.Join(dst, item.DestinationKey))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("copied content mismatch: %v", err)
		}
	}
}
