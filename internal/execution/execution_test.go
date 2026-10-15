package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

var bg = context.Background()

type fixture struct {
	t              *testing.T
	src, dst, path string
	db             *state.Store
	repo           *Repository
	engine         *Engine
	job            Job
}

func newFixture(t *testing.T, count int) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{t: t, src: filepath.Join(base, "source"), dst: filepath.Join(base, "target"), path: filepath.Join(base, "db", "agent.db")}
	for _, dir := range []string{f.src, f.dst} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
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
	f.job = Job{ChildID: "224-1", ParentID: 224, Number: 1, Generation: 1, Source: Endpoint{Kind: "LOCAL", Root: f.src}, Destination: Endpoint{Kind: "LOCAL", Root: f.dst}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true}
	source, err := transferstore.NewLocal(f.src)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := range count {
		key := fmt.Sprintf("file-%03d.bin", i)
		data := bytes.Repeat([]byte{byte(i + 1)}, i*17)
		if err := os.WriteFile(filepath.Join(f.src, key), data, 0600); err != nil {
			t.Fatal(err)
		}
		obj, err := source.Stat(bg, key)
		if err != nil {
			t.Fatal(err)
		}
		f.job.Items = append(f.job.Items, Item{ID: fmt.Sprint(i + 1), Source: obj, DestinationKey: "copied/" + key})
	}
	t.Cleanup(func() {
		if f.engine != nil {
			ctx, cancel := context.WithTimeout(bg, 5*time.Second)
			defer cancel()
			if err := f.engine.Close(ctx); err != nil {
				t.Errorf("close engine: %v", err)
			}
		}
		_ = f.db.Close()
	})
	return f
}

func (f *fixture) factory() StoreFactory {
	return StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, err := transferstore.NewLocal(job.Source.Root)
		if err != nil {
			return nil, nil, err
		}
		dst, err := transferstore.NewLocal(job.Destination.Root)
		if err != nil {
			src.Close()
			return nil, nil, err
		}
		return src, dst, nil
	})
}
func (f *fixture) startEngine(factory StoreFactory, opts Options) {
	f.t.Helper()
	if factory == nil {
		factory = f.factory()
	}
	var err error
	f.engine, err = NewEngine(f.repo, factory, opts)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) acceptStart() {
	f.t.Helper()
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		f.t.Fatal(err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, f.job.Generation); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) wait() Record {
	f.t.Helper()
	// Engine.Wait synchronizes with the execution's completion channel. This is
	// a deadlock guard, not a throughput assertion: real per-file fsyncs can take
	// longer on virtual disks without changing durability or batching semantics.
	deadline := time.Now().Add(time.Minute)
	if testDeadline, ok := f.t.Deadline(); ok && testDeadline.Add(-5*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-5 * time.Second)
	}
	ctx, cancel := context.WithDeadline(bg, deadline)
	defer cancel()
	r, err := f.engine.Wait(ctx, f.job.ChildID)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) assertFiles() {
	f.t.Helper()
	for _, item := range f.job.Items {
		want, err := os.ReadFile(filepath.Join(f.src, item.Source.Key))
		if err != nil {
			f.t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(f.dst, item.DestinationKey))
		if err != nil {
			f.t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			f.t.Fatalf("content mismatch for %s", item.ID)
		}
	}
}

type observedDB struct {
	db         *state.Store
	updates    atomic.Int64
	failLedger atomic.Bool
	failAll    atomic.Bool
	child      string
}

func (d *observedDB) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *observedDB) Update(fn func(*state.Tx) error) error {
	d.updates.Add(1)
	return d.db.Update(func(tx *state.Tx) error {
		before, _ := load(tx, d.child)
		if err := fn(tx); err != nil {
			return err
		}
		after, _ := load(tx, d.child)
		if d.failAll.Load() {
			return errors.New("injected durable commit failure")
		}
		if after.Counters.Succeeded+after.Counters.Skipped > before.Counters.Succeeded+before.Counters.Skipped && d.failLedger.CompareAndSwap(true, false) {
			return errors.New("injected ledger commit failure")
		}
		return nil
	})
}

func TestRealLocalCopyDurableLedgerAndBatchedCommits(t *testing.T) {
	f := newFixture(t, 75)
	observed := &observedDB{db: f.db, child: f.job.ChildID}
	f.repo, _ = NewRepository(observed, RepositoryOptions{})
	f.startEngine(nil, Options{CommitBatchSize: 16, CommitInterval: time.Hour})
	f.acceptStart()
	record := f.wait()
	if record.Status != Completed || record.Counters.Succeeded != 75 || record.Counters.Failed != 0 {
		t.Fatalf("unexpected record: %+v", record)
	}
	f.assertFiles()
	// 75 separate ledger fsyncs would exceed this bound. Target file fsyncs are
	// intentionally independent and cannot be removed by batching the ledger.
	if n := observed.updates.Load(); n > 10 {
		t.Fatalf("ledger was not batched: %d transactions", n)
	}
	again, err := f.engine.Accept(bg, f.job)
	if err != nil || again.Counters.Succeeded != 75 {
		t.Fatalf("replayed acceptance lost ledger: %v %+v", err, again.Counters)
	}
	changedJob := f.job
	changedJob.Items = append([]Item(nil), f.job.Items...)
	changedJob.Items[0].DestinationKey = "different"
	if _, err := f.engine.Accept(bg, changedJob); !errors.Is(err, ErrConflict) {
		t.Fatalf("different work accepted under same child: %v", err)
	}
}

func TestAcceptTransactionFailureDoesNotACKOrCreateParent(t *testing.T) {
	f := newFixture(t, 1)
	db := &observedDB{db: f.db, child: f.job.ChildID}
	db.failAll.Store(true)
	f.repo, _ = NewRepository(db, RepositoryOptions{})
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err == nil {
		t.Fatal("accepted uncommitted work")
	}
	if _, err := f.repo.Get(bg, f.job.ChildID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial job persisted: %v", err)
	}
	if _, err := f.repo.Parent(bg, f.job.ParentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parent was not rolled back: %v", err)
	}
	db.failAll.Store(false)
	f.acceptStart()
	if f.wait().Status != Completed {
		t.Fatal("retry did not complete")
	}
}

func TestPendingCapacityAndLateParentDelivery(t *testing.T) {
	f := newFixture(t, 1)
	f.repo, _ = NewRepository(f.db, RepositoryOptions{MaxPendingJobs: 1})
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	other := f.job
	other.ParentID = 225
	other.ChildID = "225-1"
	if _, err := f.engine.Accept(bg, other); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity accepted: %v", err)
	}
	if err := f.engine.StopParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrStopped) {
		t.Fatalf("late stopped accept: %v", err)
	}
	if _, err := f.engine.Accept(bg, other); !errors.Is(err, ErrCapacity) {
		t.Fatalf("stopped job lost reserved capacity: %v", err)
	}
	if err := f.engine.DeleteParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Accept(bg, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted parent resurrected: %v", err)
	}
	if err := f.engine.StopParent(bg, 999, 8); err != nil {
		t.Fatal(err)
	}
	late := f.job
	late.ParentID = 999
	late.ChildID = "999-1"
	late.Generation = 8
	if _, err := f.engine.Accept(bg, late); !errors.Is(err, ErrStopped) {
		t.Fatalf("pre-accept fence failed: %v", err)
	}
}

func TestQueueEmptyDoesNotCompleteBeforeDiscoveryEnd(t *testing.T) {
	f := newFixture(t, 4)
	f.job.DiscoveryComplete = false
	f.startEngine(nil, Options{QueueSize: 1})
	f.acceptStart()
	r := f.wait()
	if r.Status != Waiting || r.Counters.Succeeded != 4 {
		t.Fatalf("premature completion: %+v", r)
	}
	r, err := f.engine.Seal(bg, f.job.ChildID, 1)
	if err != nil || r.Status != Completed {
		t.Fatalf("seal did not finish: %v %s", err, r.Status)
	}
	r, err = f.engine.Accept(bg, f.job)
	if err != nil || !r.Job.DiscoveryComplete || r.Status != Completed {
		t.Fatalf("old acceptance unsealed job: %v %+v", err, r)
	}
}

type delayedWrite struct {
	transferstore.Store
	once             *atomic.Bool
	entered, release chan struct{}
}

func (s *delayedWrite) Write(ctx context.Context, key string, input io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	result, err := s.Store.Write(ctx, key, input, opts)
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return result, err
}

func TestStopFencePrecedesCancelAndResumeWaitsOldAttempt(t *testing.T) {
	f := newFixture(t, 1)
	f.job.ConflictPolicy = "SKIP"
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		s, d, err := base.Open(ctx, j)
		return s, &delayedWrite{Store: d, once: &first, entered: entered, release: release}, err
	}), Options{WorkersPerJob: 1, QueueSize: 1})
	f.acceptStart()
	<-entered
	before, _ := f.repo.Get(bg, f.job.ChildID)
	stopCtx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	if err := f.engine.Stop(stopCtx, f.job.ChildID, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop must wait blocked I/O: %v", err)
	}
	stopped, _ := f.repo.Get(bg, f.job.ChildID)
	if stopped.Status != Stopped || stopped.Attempt <= before.Attempt {
		t.Fatalf("stop fence missing: %+v", stopped)
	}
	resumeCtx, done := context.WithTimeout(bg, 30*time.Millisecond)
	defer done()
	if err := f.engine.Resume(resumeCtx, f.job.ChildID, 1, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new attempt overtook blocked previous I/O: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	r := f.wait()
	if r.Status != Stopped || r.Counters.Succeeded != 0 {
		t.Fatalf("old result crossed stop fence: %+v", r)
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	r = f.wait()
	if r.Status != Completed || r.Counters.Skipped != 1 || r.Counters.Succeeded != 0 {
		t.Fatalf("at-least-once recovery did not honor SKIP: %+v", r)
	}
	f.assertFiles()
}

func TestParentGenerationRejectsOldResultAndOldAccept(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	_, _ = f.engine.Accept(bg, f.job)
	old, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.ResumeParent(bg, 224, 2, true); err != nil {
		t.Fatalf("committed resume was not idempotent: %v", err)
	}
	if _, err := f.repo.commit(bg, f.job.ChildID, 1, old.Attempt, []ItemResult{{ID: "1", Status: ItemSucceeded}}); !errors.Is(err, ErrGeneration) {
		t.Fatalf("old result accepted: %v", err)
	}
	if _, err := f.engine.Accept(bg, f.job); !errors.Is(err, ErrGeneration) {
		t.Fatalf("old accept accepted: %v", err)
	}
	f.job.Generation = 2
	f.acceptStart()
	r := f.wait()
	if r.Status != Completed || r.Counters.Succeeded != 1 {
		t.Fatalf("new generation failed: %+v", r)
	}
}

func TestPendingIndexTracksAcceptBeginStopSealAndRecovery(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job); err != nil {
		t.Fatal(err)
	}
	pending, err := f.repo.Pending(bg, "", 16)
	if err != nil || len(pending) != 1 {
		t.Fatalf("accepted job missing from ready index: %v %+v", err, pending)
	}
	if _, err := f.repo.begin(bg, f.job.ChildID, 1, false, false); err != nil {
		t.Fatal(err)
	}
	if pending, err = f.repo.Pending(bg, "", 16); err != nil || len(pending) != 0 {
		t.Fatalf("running job remained ready: %v %+v", err, pending)
	}
	if err := f.repo.Recover(bg); err != nil {
		t.Fatal(err)
	}
	if pending, err = f.repo.Pending(bg, "", 16); err != nil || len(pending) != 1 || pending[0].Status != Recoverable {
		t.Fatalf("recovered job missing: %v %+v", err, pending)
	}
	if err := f.engine.Stop(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if pending, err = f.repo.Pending(bg, "", 16); err != nil || len(pending) != 0 {
		t.Fatalf("stopped job remained ready: %v %+v", err, pending)
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	if f.wait().Status != Completed {
		t.Fatal("resume failed")
	}
	if pending, err = f.repo.Pending(bg, "", 16); err != nil || len(pending) != 0 {
		t.Fatalf("completed history remained ready: %v %+v", err, pending)
	}
	empty := f.job
	empty.ChildID = "224-2"
	empty.Number = 2
	empty.Items = nil
	empty.DiscoveryComplete = false
	if _, err := f.engine.Accept(bg, empty); err != nil {
		t.Fatal(err)
	}
	if pending, err = f.repo.Pending(bg, "", 16); err != nil || len(pending) != 0 {
		t.Fatalf("unsealed empty job would repeatedly spin: %v %+v", err, pending)
	}
	if result, err := f.engine.Seal(bg, empty.ChildID, 1); err != nil || result.Status != Completed {
		t.Fatalf("empty job seal failed: %v %+v", err, result)
	}
}

func TestStopParentCancelsEveryActiveChild(t *testing.T) {
	f := newFixture(t, 2)
	var ready atomic.Int64
	allReady := make(chan struct{})
	var closed atomic.Int64
	f.startEngine(nil, Options{MaxConcurrentJobs: 2, WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		if ready.Add(1) == 2 {
			close(allReady)
		}
		return &cancelReader{input: input, ctx: ctx, closed: &closed}, nil
	}})
	f.acceptStart()
	second := f.job
	second.ChildID = "224-2"
	second.Number = 2
	if _, err := f.engine.Accept(bg, second); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(bg, second.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-allReady:
	case <-time.After(5 * time.Second):
		t.Fatal("children did not start concurrently")
	}
	if err := f.engine.StopParent(bg, 224, 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.job.ChildID, second.ChildID} {
		r, _ := f.repo.Get(bg, id)
		if r.Status != Stopped || r.Counters.Succeeded != 0 {
			t.Fatalf("child escaped parent stop: %+v", r)
		}
	}
	if closed.Load() != 2 {
		t.Fatalf("leaked readers after stop: %d", closed.Load())
	}
}

func TestLedgerFailureAfterWriteRecoversAccordingToConflictPolicy(t *testing.T) {
	for _, policy := range []string{"SKIP", "OVERWRITE"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t, 2)
			f.job.ConflictPolicy = policy
			observed := &observedDB{db: f.db, child: f.job.ChildID}
			observed.failLedger.Store(true)
			f.repo, _ = NewRepository(observed, RepositoryOptions{})
			f.startEngine(nil, Options{CommitBatchSize: 128, CommitInterval: time.Hour})
			f.acceptStart()
			r := f.wait()
			if r.Status != Recoverable || r.Counters.Succeeded != 0 || r.Counters.Skipped != 0 || !strings.Contains(r.Error, "ledger commit failure") {
				t.Fatalf("failed commit leaked ledger or counters: %+v", r)
			}
			f.assertFiles()
			if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
				t.Fatal(err)
			}
			r = f.wait()
			if r.Status != Completed {
				t.Fatalf("resume failed: %+v", r)
			}
			if policy == "SKIP" && r.Counters.Skipped != 2 {
				t.Fatalf("SKIP recovery overwrote data: %+v", r.Counters)
			}
			if policy == "OVERWRITE" && r.Counters.Succeeded != 2 {
				t.Fatalf("OVERWRITE recovery skipped write: %+v", r.Counters)
			}
		})
	}
}

type replacedReader struct {
	io.Reader
	io.Closer
}
type malformedSource struct {
	transferstore.Store
	delta int
}

func (s *malformedSource) Open(ctx context.Context, key string, opts transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	original, obj, err := s.Store.Open(ctx, key, opts)
	if err != nil {
		return nil, obj, err
	}
	payload := bytes.Repeat([]byte("x"), int(obj.Size)+s.delta)
	return &replacedReader{Reader: bytes.NewReader(payload), Closer: original}, obj, nil
}

func TestShortAndOversizedSourceFailWithoutPublishing(t *testing.T) {
	for _, delta := range []int{-1, 1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			f := newFixture(t, 2)
			f.job.Items = f.job.Items[1:]
			base := f.factory()
			f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
				s, d, err := base.Open(ctx, j)
				return &malformedSource{Store: s, delta: delta}, d, err
			}), Options{})
			f.acceptStart()
			r := f.wait()
			if r.Status != Failed || r.Counters.Failed != 1 || r.Results[0].Error == "" {
				t.Fatalf("invalid source reported success: %+v", r)
			}
			if _, err := os.Stat(filepath.Join(f.dst, f.job.Items[0].DestinationKey)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete object published: %v", err)
			}
		})
	}
}

func TestSourceChangedAfterDiscoveryIsRecorded(t *testing.T) {
	f := newFixture(t, 2)
	f.job.Items = f.job.Items[1:]
	if err := os.WriteFile(filepath.Join(f.src, f.job.Items[0].Source.Key), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	f.acceptStart()
	r := f.wait()
	if r.Status != Failed || !strings.Contains(r.Results[0].Error, "source changed") {
		t.Fatalf("source mismatch hidden: %+v", r)
	}
}

type failedSource struct {
	transferstore.Store
	fail  *atomic.Bool
	opens *atomic.Int64
}

func (s *failedSource) Open(ctx context.Context, key string, opts transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	s.opens.Add(1)
	if strings.Contains(key, "001") && s.fail.Load() {
		return nil, transferstore.Object{}, errors.New("source read permission denied")
	}
	return s.Store.Open(ctx, key, opts)
}
func TestRetryFailedKeepsSuccessfulLedger(t *testing.T) {
	f := newFixture(t, 3)
	var fail atomic.Bool
	fail.Store(true)
	var opens atomic.Int64
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		s, d, err := base.Open(ctx, j)
		return &failedSource{Store: s, fail: &fail, opens: &opens}, d, err
	}), Options{})
	f.acceptStart()
	r := f.wait()
	if r.Status != PartialFailed || r.Counters.Succeeded != 2 || r.Counters.Failed != 1 || !strings.Contains(r.Results[1].Error, "permission denied") {
		t.Fatalf("failure not retained: %+v", r)
	}
	fail.Store(false)
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	if f.wait().Status != PartialFailed || opens.Load() != 3 {
		t.Fatal("failed object retried without request")
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 1, true); err != nil {
		t.Fatal(err)
	}
	r = f.wait()
	if r.Status != Completed || r.Counters.Succeeded != 3 || r.Counters.Failed != 0 || opens.Load() != 4 {
		t.Fatalf("retry duplicated successful work: %+v, opens=%d", r, opens.Load())
	}
	f.assertFiles()
}

func TestRestartRestoresRunningButPreservesStopAndRevalidatesLedger(t *testing.T) {
	f := newFixture(t, 2)
	f.startEngine(nil, Options{})
	f.acceptStart()
	if f.wait().Status != Completed {
		t.Fatal("initial copy failed")
	}
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	// Leave one durable RUNNING record as an abruptly terminated process would.
	if _, err := f.repo.begin(bg, f.job.ChildID, 1, true, false); err != nil {
		t.Fatal(err)
	}
	other := f.job
	other.ChildID = "224-2"
	other.Number = 2
	if _, err := f.repo.Accept(bg, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.stop(bg, other.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, _ = NewRepository(f.db, RepositoryOptions{})
	f.startEngine(nil, Options{})
	r, _ := f.repo.Get(bg, f.job.ChildID)
	if r.Status != Recoverable || r.Counters.Succeeded != 2 {
		t.Fatalf("running job not recovered: %+v", r)
	}
	r, _ = f.repo.Get(bg, other.ChildID)
	if r.Status != Stopped {
		t.Fatalf("restart resumed stopped job: %+v", r)
	}
	if err := os.Remove(filepath.Join(f.dst, f.job.Items[1].DestinationKey)); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	r = f.wait()
	if r.Status != Completed || r.Counters.Succeeded != 2 {
		t.Fatalf("missing completed target not repaired: %+v", r)
	}
	f.assertFiles()
}

type blockingReader struct {
	io.ReadCloser
	entered, release chan struct{}
	once             sync.Once
	ctx              context.Context
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		return r.ReadCloser.Read(p)
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func TestStartRPCContextDoesNotOwnExecutionAndSlotsBackpressure(t *testing.T) {
	f := newFixture(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.startEngine(nil, Options{MaxConcurrentJobs: 1, WorkersPerJob: 1, QueueSize: 1, DecorateReader: func(ctx context.Context, j Job, item Item, input io.ReadCloser) (io.ReadCloser, error) {
		if first.CompareAndSwap(false, true) {
			return &blockingReader{ReadCloser: input, entered: entered, release: release, ctx: ctx}, nil
		}
		return input, nil
	}})
	_, _ = f.engine.Accept(bg, f.job)
	rpc, cancel := context.WithCancel(bg)
	if err := f.engine.Start(rpc, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-entered
	other := f.job
	other.ChildID = "224-2"
	other.Number = 2
	if _, err := f.engine.Accept(bg, other); err != nil {
		t.Fatal(err)
	}
	limited, done := context.WithTimeout(bg, 30*time.Millisecond)
	defer done()
	if err := f.engine.Start(limited, other.ChildID, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slot pool did not backpressure: %v", err)
	}
	still, _ := f.repo.Get(bg, other.ChildID)
	if still.Status != Waiting {
		t.Fatalf("accepted work dropped on capacity pressure: %+v", still)
	}
	close(release)
	if f.wait().Status != Completed {
		t.Fatal("RPC cancel stopped real task")
	}
	if err := f.engine.Start(bg, other.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	ctx, finish := context.WithTimeout(bg, 5*time.Second)
	defer finish()
	if _, err := f.engine.Wait(ctx, other.ChildID); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCancelsWorkersAndPreservesRecoverableWork(t *testing.T) {
	f := newFixture(t, 8)
	var opened, closed atomic.Int64
	ready := make(chan struct{})
	var once sync.Once
	f.startEngine(nil, Options{WorkersPerJob: 2, QueueSize: 1, DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		opened.Add(1)
		once.Do(func() { close(ready) })
		return &cancelReader{input: input, ctx: ctx, closed: &closed}, nil
	}})
	f.acceptStart()
	<-ready
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := f.engine.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := f.repo.Get(bg, f.job.ChildID)
	if r.Status != Recoverable || opened.Load() != closed.Load() {
		t.Fatalf("close leaked I/O or completed interrupted job: %+v opened=%d closed=%d", r, opened.Load(), closed.Load())
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed engine started job: %v", err)
	}
}

type cancelReader struct {
	input  io.ReadCloser
	ctx    context.Context
	closed *atomic.Int64
	once   sync.Once
}

func (r *cancelReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r *cancelReader) Close() error             { r.once.Do(func() { r.closed.Add(1) }); return r.input.Close() }
