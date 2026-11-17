package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type progressWriteGate struct {
	entered, read, halfway, rest, finalizing, commit chan struct{}
	fail                                             bool
}

func newProgressWriteGate() *progressWriteGate {
	return &progressWriteGate{entered: make(chan struct{}), read: make(chan struct{}), halfway: make(chan struct{}),
		rest: make(chan struct{}), finalizing: make(chan struct{}), commit: make(chan struct{})}
}

type progressGatedStore struct {
	transferstore.Store
	gate *progressWriteGate
}

func (s *progressGatedStore) Write(ctx context.Context, key string, reader io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if opts.Progress != nil {
		// Only a store that reports its own progress may receive the counter.
		return transferstore.WriteResult{}, errors.New("progress passed to a store that does not report it")
	}
	close(s.gate.entered)
	if err := wait(ctx, s.gate.read); err != nil {
		return transferstore.WriteResult{}, err
	}
	var data bytes.Buffer
	if _, err := io.CopyN(&data, reader, opts.Size/2); err != nil {
		return transferstore.WriteResult{}, err
	}
	close(s.gate.halfway)
	if err := wait(ctx, s.gate.rest); err != nil {
		return transferstore.WriteResult{}, err
	}
	if _, err := io.Copy(&data, reader); err != nil {
		return transferstore.WriteResult{}, err
	}
	close(s.gate.finalizing)
	if err := wait(ctx, s.gate.commit); err != nil {
		return transferstore.WriteResult{}, err
	}
	if s.gate.fail {
		return transferstore.WriteResult{}, errors.New("injected finalization failure")
	}
	return s.Store.Write(ctx, key, bytes.NewReader(data.Bytes()), opts)
}

// reportingGatedStore reports its writes' progress the way a destination
// does: it reads the whole source, hands it to the "network" (Sending), and
// confirms it (Accepted) only when the gate allows.
type reportingGatedStore struct {
	transferstore.Store
	gate *progressWriteGate
}

func (s *reportingGatedStore) ReportsProgress() bool { return true }

func (s *reportingGatedStore) Write(ctx context.Context, key string, reader io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if opts.Progress == nil {
		return transferstore.WriteResult{}, errors.New("a reporting store received no progress counter")
	}
	close(s.gate.entered)
	if err := wait(ctx, s.gate.read); err != nil {
		return transferstore.WriteResult{}, err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return transferstore.WriteResult{}, err
	}
	// A failed attempt hands bytes to the network and withdraws them.
	opts.Progress.Sending(int64(len(data)) / 2)
	opts.Progress.Sending(-int64(len(data)) / 2)
	opts.Progress.Sending(int64(len(data)))
	opts.Progress.Sent(int64(len(data))/2 + int64(len(data)))
	close(s.gate.halfway)
	if err := wait(ctx, s.gate.rest); err != nil {
		return transferstore.WriteResult{}, err
	}
	opts.Progress.Accepted(int64(len(data)), int64(len(data)))
	close(s.gate.finalizing)
	if err := wait(ctx, s.gate.commit); err != nil {
		return transferstore.WriteResult{}, err
	}
	opts.Progress = nil
	return s.Store.Write(ctx, key, bytes.NewReader(data), opts)
}

func awaitProgressGate(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not reach its progress checkpoint")
	}
}

func setProgressSource(t *testing.T, f *fixture, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.src, f.job.Items[0].Source.Key), bytes.Repeat([]byte("x"), size), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := transferstore.NewLocal(f.src)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f.job.Items[0].Source, err = source.Stat(bg, f.job.Items[0].Source.Key)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLiveObjectProgressReadsFinalizesAndCleansUpWithoutLedgerWrites(t *testing.T) {
	f := newFixture(t, 1)
	setProgressSource(t, f, 2<<20)
	f.job.Assignment = 7
	observed := &observedDB{db: f.db, child: f.job.ChildID}
	f.repo, _ = NewRepository(observed, RepositoryOptions{})
	gate := newProgressWriteGate()
	base := f.factory()
	var read atomic.Int64
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		return src, &progressGatedStore{Store: dst, gate: gate}, err
	}), Options{CountTransferred: func(_ Job, n int64) { read.Add(n) }})
	f.acceptStart()
	awaitProgressGate(t, gate.entered)
	before := observed.updates.Load()
	close(gate.read)
	awaitProgressGate(t, gate.halfway)
	for range 50 {
		child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
		if !ok || child.Assignment != 7 || child.Attempt != 1 || child.Generation != 1 || child.ActiveObjects != 1 || child.SucceededBytes != 0 || child.InFlightBytes != 1<<20 || len(items) != 1 || items[0].TransferredBytes != 1<<20 || items[0].Stage != "TRANSFERRING" ||
			items[0].ReadBytes != 1<<20 || items[0].Basis != BasisSource || items[0].Phase != "" {
			t.Fatalf("halfway progress: %+v %+v %v", child, items, ok)
		}
		rows := f.engine.Progress(f.job.ParentID, 1)
		if len(rows) != 1 || rows[0] != child {
			t.Fatalf("summary differs from detail: %+v %+v", rows, child)
		}
	}
	if observed.updates.Load() != before {
		t.Fatal("live reads or progress snapshots wrote the ledger")
	}
	// A target that does not report its progress feeds the speed its reads.
	if got := read.Load(); got != 1<<20 {
		t.Fatalf("speed counted %d bytes of reads, want %d", got, 1<<20)
	}
	close(gate.rest)
	awaitProgressGate(t, gate.finalizing)
	child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
	if !ok || child.SucceededBytes != 0 || child.InFlightBytes != 2<<20 || items[0].Stage != "FINALIZING" {
		t.Fatalf("source exhaustion falsely confirmed target completion: %+v %+v", child, items)
	}
	if observed.updates.Load() != before {
		t.Fatal("source exhaustion wrote the ledger before target confirmation")
	}
	close(gate.commit)
	if record := f.wait(); record.Status != Completed || record.Counters.SucceededBytes != 2<<20 {
		t.Fatalf("confirmed bytes not persisted: %+v", record)
	}
	f.assertFiles()
	if rows := f.engine.Progress(f.job.ParentID, 1); len(rows) != 0 {
		t.Fatal("completed attempt retained progress", rows)
	}
	if _, _, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID); ok {
		t.Fatal("completed object retained progress")
	}
}

// A destination that reports its own progress counts what it confirmed plus
// what is in flight: a source read to the end is not a finished transfer,
// and FINALIZING appears only once the destination confirmed every byte.
func TestLiveObjectProgressCountsTargetConfirmation(t *testing.T) {
	f := newFixture(t, 1)
	setProgressSource(t, f, 2<<20)
	gate := newProgressWriteGate()
	base := f.factory()
	var sent atomic.Int64
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		return src, &reportingGatedStore{Store: dst, gate: gate}, err
	}), Options{CountTransferred: func(job Job, n int64) {
		if job.ParentID == f.job.ParentID && job.Generation == 1 && job.ChildID == f.job.ChildID && len(job.Items) == 0 {
			sent.Add(n)
		}
	}})
	f.acceptStart()
	awaitProgressGate(t, gate.entered)
	child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
	if !ok || child.InFlightBytes != 0 || len(items) != 1 || items[0].TransferredBytes != 0 || items[0].Stage != "TRANSFERRING" {
		t.Fatalf("before the write: %+v %+v %v", child, items, ok)
	}
	close(gate.read)
	awaitProgressGate(t, gate.halfway)
	// The whole source was read and handed on, but nothing is confirmed.
	child, items, ok = f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
	if !ok || child.InFlightBytes != 2<<20-1 || len(items) != 1 || items[0].TransferredBytes != 2<<20-1 || items[0].Stage != "TRANSFERRING" ||
		items[0].ReadBytes != 2<<20 || items[0].Basis != BasisTarget || items[0].Phase != "" {
		t.Fatalf("unconfirmed bytes finalized the object: %+v %+v %v", child, items, ok)
	}
	// The speed counts what the target sent, the failed attempt's bytes
	// included, not what was read from the source.
	if got := sent.Load(); got != 3<<20 {
		t.Fatalf("speed counted %d bytes, want %d", got, 3<<20)
	}
	if rows := f.engine.Progress(f.job.ParentID, 1); len(rows) != 1 || rows[0] != child {
		t.Fatalf("summary differs from detail: %+v %+v", rows, child)
	}
	close(gate.rest)
	awaitProgressGate(t, gate.finalizing)
	child, items, ok = f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
	if !ok || child.InFlightBytes != 2<<20 || child.SucceededBytes != 0 || len(items) != 1 || items[0].TransferredBytes != 2<<20 || items[0].Stage != "FINALIZING" {
		t.Fatalf("confirmed bytes did not finalize the object: %+v %+v %v", child, items, ok)
	}
	close(gate.commit)
	if record := f.wait(); record.Status != Completed || record.Counters.SucceededBytes != 2<<20 {
		t.Fatalf("confirmed bytes not persisted: %+v", record)
	}
	f.assertFiles()
}

// Counted at the destination, an object reaches its size only when every
// byte is confirmed, and a withdrawn attempt lowers it again: no peak is kept.
func TestObjectProgressCountsDestinationConfirmation(t *testing.T) {
	object := &objectProgress{item: ObjectProgress{Size: 1000}, targetCounted: true}
	var progress transferstore.WriteProgress = object
	steps := []struct {
		name string
		do   func()
		want int64
	}{
		{"first part sending", func() { progress.Sending(600) }, 600},
		{"second part sent in full", func() { progress.Sending(400) }, 999},
		{"second part confirmed", func() { progress.Accepted(400, 400) }, 999},
		{"first attempt failed", func() { progress.Sending(-600) }, 400},
		{"retry sending", func() { progress.Sending(250) }, 650},
		{"retry sent", func() { progress.Sending(350) }, 999},
		{"retry confirmed", func() { progress.Accepted(600, 600) }, 1000},
	}
	for _, step := range steps {
		step.do()
		if got := object.transferred(); got != step.want {
			t.Fatalf("%s: transferred %d, want %d", step.name, got, step.want)
		}
	}
	empty := &objectProgress{targetCounted: true}
	if got := empty.transferred(); got != 0 {
		t.Fatalf("empty object transferred %d", got)
	}
	p := newAttemptProgress(bg, Record{Job: Job{Items: []Item{{ID: "a", Source: transferstore.Object{Size: 1000}}, {ID: "b"}}}}, nil)
	p.initialize(0, true)
	live := p.begin(Item{ID: "a", Source: transferstore.Object{Size: 1000}})
	p.begin(Item{ID: "b"})
	live.Sending(1000)
	if child, items, _ := p.snapshot(true); child.InFlightBytes != 999 || items[0].Stage != "TRANSFERRING" || items[1].Stage != "FINALIZING" {
		t.Fatal("unconfirmed or empty object staged wrongly", child, items)
	}
	live.Sending(-1000)
	if child, _, _ := p.snapshot(false); child.InFlightBytes != 0 {
		t.Fatal("withdrawn bytes stayed in the summary", child)
	}
}

// The phase shows what an object waits for only while none of its requests
// is sending: a failed request outranks the request limit, which outranks
// memory. Progress counted from source reads has no phase.
func TestObjectProgressPhase(t *testing.T) {
	object := &objectProgress{item: ObjectProgress{Size: 100}, targetCounted: true}
	steps := []struct {
		phase transferstore.WritePhase
		delta int
		want  string
	}{
		{transferstore.PhaseUploadBuffer, 1, PhaseUploadBuffer},
		{transferstore.PhaseRequestLimit, 1, PhaseRequestLimit},
		{transferstore.PhaseRetryWait, 1, PhaseRetryWait},
		{transferstore.PhaseSending, 1, ""},
		{transferstore.PhaseSending, -1, PhaseRetryWait},
		{transferstore.PhaseRetryWait, -1, PhaseRequestLimit},
		{transferstore.PhaseRequestLimit, -1, PhaseUploadBuffer},
		{transferstore.PhaseUploadBuffer, -1, ""},
		{transferstore.WritePhase(99), 1, ""},
	}
	for i, step := range steps {
		object.Phase(step.phase, step.delta)
		if got := object.phase(); got != step.want {
			t.Fatalf("step %d: phase %q, want %q", i, got, step.want)
		}
	}
	source := &objectProgress{item: ObjectProgress{Size: 100}}
	source.Phase(transferstore.PhaseRetryWait, 1)
	if got := source.phase(); got != "" {
		t.Fatalf("source-counted phase %q", got)
	}
}

// Every object's speed reaches Options.CountTransferred with the identity of
// its child task only: a local destination reports the bytes it wrote.
func TestCountTransferredReportsBytesWrittenToTheDestination(t *testing.T) {
	f := newFixture(t, 4)
	var total atomic.Int64
	f.startEngine(nil, Options{CountTransferred: func(job Job, n int64) {
		if job.ParentID != f.job.ParentID || job.Generation != f.job.Generation || job.ChildID != f.job.ChildID || job.Items != nil || n <= 0 {
			t.Errorf("counted %d bytes for %+v", n, job)
		}
		total.Add(n)
	}})
	f.acceptStart()
	if record := f.wait(); record.Status != Completed {
		t.Fatal(record)
	}
	f.assertFiles()
	var want int64
	for _, item := range f.job.Items {
		want += item.Source.Size
	}
	if total.Load() != want {
		t.Fatalf("speed counted %d bytes, the destination received %d", total.Load(), want)
	}
}

// A request's confirmation moves its bytes from pending to accepted in one
// step: a snapshot taken meanwhile neither loses them nor counts them twice.
func TestObjectProgressConfirmationIsOneStep(t *testing.T) {
	const size, request = int64(1 << 40), int64(4096)
	object := &objectProgress{item: ObjectProgress{Size: size}, targetCounted: true}
	var floor atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for counted := int64(0); counted < 20000*request; counted += request {
			object.Sending(request)
			floor.Store(counted + request)
			object.Accepted(request, request)
		}
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		least := floor.Load()
		got := object.transferred()
		// At most the request sent since floor was last raised.
		if most := floor.Load() + request; got < least || got > most {
			t.Fatalf("snapshot %d outside [%d, %d]", got, least, most)
		}
	}
}

func TestLiveObjectProgressFailureRetryGenerationAndCancellation(t *testing.T) {
	f := newFixture(t, 1)
	setProgressSource(t, f, 64<<10)
	gates := []*progressWriteGate{newProgressWriteGate(), newProgressWriteGate(), newProgressWriteGate()}
	gates[0].fail = true
	var opened atomic.Int64
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		return src, &progressGatedStore{Store: dst, gate: gates[opened.Add(1)-1]}, err
	}), Options{})
	f.acceptStart()
	awaitProgressGate(t, gates[0].entered)
	close(gates[0].read)
	close(gates[0].rest)
	awaitProgressGate(t, gates[0].finalizing)
	close(gates[0].commit)
	if record := f.wait(); record.Status != Failed || record.Counters.SucceededBytes != 0 {
		t.Fatal("failed finalization credited bytes", record)
	}
	if len(f.engine.Progress(f.job.ParentID, 1)) != 0 {
		t.Fatal("failed attempt retained progress")
	}
	if err := f.engine.ResumeParent(bg, f.job.ParentID, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 2, true); err != nil {
		t.Fatal(err)
	}
	awaitProgressGate(t, gates[1].entered)
	child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 2, f.job.ChildID)
	if !ok || child.Generation != 2 || child.Attempt <= 1 || child.InFlightBytes != 0 || child.SucceededBytes != 0 || len(items) != 1 || items[0].TransferredBytes != 0 {
		t.Fatalf("retry inherited failed bytes: %+v %+v", child, items)
	}
	retryAttempt := child.Attempt
	if len(f.engine.Progress(f.job.ParentID, 1)) != 0 {
		t.Fatal("old generation matched live retry")
	}
	if _, _, ok := f.engine.ProgressObjects(f.job.ParentID+1, 2, f.job.ChildID); ok {
		t.Fatal("another parent matched live child")
	}
	close(gates[1].read)
	awaitProgressGate(t, gates[1].halfway)
	if err := f.engine.Stop(bg, f.job.ChildID, 2); err != nil {
		t.Fatal(err)
	}
	if len(f.engine.Progress(f.job.ParentID, 2)) != 0 {
		t.Fatal("cancelled attempt retained live bytes")
	}
	if err := f.engine.Resume(bg, f.job.ChildID, 2, true); err != nil {
		t.Fatal(err)
	}
	awaitProgressGate(t, gates[2].entered)
	child, _, ok = f.engine.ProgressObjects(f.job.ParentID, 2, f.job.ChildID)
	if !ok || child.Attempt <= retryAttempt || child.InFlightBytes != 0 || child.SucceededBytes != 0 {
		t.Fatal("new attempt inherited cancelled bytes", child)
	}
	close(gates[2].read)
	close(gates[2].rest)
	close(gates[2].commit)
	if record := f.wait(); record.Status != Completed {
		t.Fatal(record)
	}
}

func TestLiveObjectProgressReconcilesSucceededBaselineBeforeRetry(t *testing.T) {
	f := newFixture(t, 2)
	setProgressSource(t, f, 64<<10)
	f.startEngine(nil, Options{})
	f.acceptStart()
	if record := f.wait(); record.Status != Completed {
		t.Fatal(record)
	}
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	// Restore a job interrupted after persisting results, then lose one target.
	// Recovery must not count that object as both succeeded and in flight.
	if _, err := f.repo.begin(bg, f.job.ChildID, 1, true, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dst, f.job.Items[0].DestinationKey)); err != nil {
		t.Fatal(err)
	}
	gate := newProgressWriteGate()
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		return src, &progressGatedStore{Store: dst, gate: gate}, err
	}), Options{})
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	awaitProgressGate(t, gate.entered)
	close(gate.read)
	awaitProgressGate(t, gate.halfway)
	child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
	if !ok || child.SucceededBytes != f.job.Items[1].Source.Size || child.InFlightBytes != 32<<10 || len(items) != 1 || items[0].ItemID != f.job.Items[0].ID {
		t.Fatal("invalidated success remained in progress baseline", child, items)
	}
	close(gate.rest)
	close(gate.commit)
	if record := f.wait(); record.Status != Completed || record.Counters.SucceededBytes != (64<<10)+f.job.Items[1].Source.Size {
		t.Fatal("repaired object was credited incorrectly", record)
	}
	f.assertFiles()
}

func TestLiveObjectProgressUses64BitSizesAndAtomicCompletionBoundary(t *testing.T) {
	// Reproduce a 349.9 GiB object without allocating its contents. The public
	// stream integration is covered above; this checks 32-bit targets as well.
	const size int64 = 3499 * (1 << 30) / 10
	job := Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 2, Assignment: 3,
		Items: []Item{{ID: "first", Source: transferstore.Object{Size: size}}, {ID: "second", Source: transferstore.Object{Size: size}}}}
	p := newAttemptProgress(bg, Record{Job: job, Attempt: 4}, nil)
	p.initialize(5, false)
	second := p.begin(job.Items[1])
	first := p.begin(job.Items[0])
	first.read.Store(size)
	second.read.Store(size / 2)
	child, items, ok := p.snapshot(true)
	if !ok || child.InFlightBytes != size+size/2 || child.SucceededBytes != 5 || len(items) != 2 || items[0].ItemID != "first" || items[1].ItemID != "second" {
		t.Fatal(child, items)
	}
	before := child.SucceededBytes + child.InFlightBytes
	p.finish(first, true)
	child, items, _ = p.snapshot(true)
	if child.SucceededBytes+child.InFlightBytes != before || child.SucceededBytes != 5+size || len(items) != 1 || items[0].ItemID != "second" {
		t.Fatal("completion double-counted or lost bytes", child, items)
	}
	p.finish(first, true)
	child, _, _ = p.snapshot(false)
	if child.SucceededBytes != 5+size {
		t.Fatal("duplicate completion credited twice", child)
	}
}

func BenchmarkProgressReader(b *testing.B) {
	data := bytes.Repeat([]byte("x"), 64<<10)
	for _, tracked := range []bool{false, true} {
		name := "baseline"
		if tracked {
			name = "progress"
		}
		b.Run(name, func(b *testing.B) {
			source := bytes.NewReader(data)
			var input io.Reader = io.NopCloser(source)
			progress := &objectProgress{}
			if tracked {
				input = &progressReader{ReadCloser: io.NopCloser(source), progress: progress}
			}
			buffer := make([]byte, len(data))
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				source.Reset(data)
				if n, err := input.Read(buffer); n != len(data) || err != nil {
					b.Fatal(n, err)
				}
			}
			b.StopTimer()
			if tracked && progress.read.Load() != int64(b.N)*int64(len(data)) {
				b.Fatal("progress reader lost bytes")
			}
		})
	}
}

// causeStore blocks every write until its context ends and reports why.
type causeStore struct {
	transferstore.Store
	entered chan struct{}
	causes  chan error
}

func (s *causeStore) Write(ctx context.Context, _ string, _ io.Reader, _ transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.entered <- struct{}{}
	<-ctx.Done()
	s.causes <- context.Cause(ctx)
	return transferstore.WriteResult{}, ctx.Err()
}

// An attempt ends with the reason it was cancelled for: a stop, a shutdown
// and an interrupted lease may continue on this Agent; a task moved to another
// Agent and a deleted task may not. Behaviour does not depend on it yet.
func TestAttemptsEndWithTheirCause(t *testing.T) {
	type trigger func(f *fixture, lease context.CancelCauseFunc) error
	for _, tc := range []struct {
		name string
		stop trigger
		want error
	}{
		{"stop child", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.Stop(bg, f.job.ChildID, 1)
		}, transferstore.ErrInterrupted},
		{"stop task", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.StopParent(bg, f.job.ParentID, 1)
		}, transferstore.ErrInterrupted},
		{"resume fence", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.ResumeParent(bg, f.job.ParentID, 2, false)
		}, transferstore.ErrInterrupted},
		{"quiesce interrupted", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.QuiesceParent(bg, f.job.ParentID, transferstore.ErrInterrupted)
		}, transferstore.ErrInterrupted},
		{"quiesce moved", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.QuiesceParent(bg, f.job.ParentID, transferstore.ErrMoved)
		}, transferstore.ErrMoved},
		{"lease moved", func(_ *fixture, lease context.CancelCauseFunc) error {
			lease(transferstore.ErrMoved)
			return nil
		}, transferstore.ErrMoved},
		{"delete", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.RequestDeleteParent(bg, f.job.ParentID, 1)
		}, transferstore.ErrTaskDeleted},
		{"shutdown", func(f *fixture, _ context.CancelCauseFunc) error {
			return f.engine.Close(bg)
		}, transferstore.ErrInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 2)
			store := &causeStore{entered: make(chan struct{}, 1), causes: make(chan error, 1)}
			base := f.factory()
			lease, cancelLease := context.WithCancelCause(bg)
			defer cancelLease(nil)
			f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
				src, dst, err := base.Open(ctx, job)
				store.Store = dst
				return src, store, err
			}), Options{WorkersPerJob: 1, JobContext: func(Job) (context.Context, error) { return lease, nil }})
			f.acceptStart()
			awaitProgressGate(t, store.entered)
			if err := tc.stop(f, cancelLease); err != nil {
				t.Fatal(err)
			}
			select {
			case cause := <-store.causes:
				if !errors.Is(cause, tc.want) {
					t.Fatalf("write ended with %v, want %v", cause, tc.want)
				}
			case <-time.After(time.Minute): // hang guard only
				t.Fatal("the write was not cancelled")
			}
		})
	}
}
