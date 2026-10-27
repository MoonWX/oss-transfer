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
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		src, dst, err := base.Open(ctx, job)
		return src, &progressGatedStore{Store: dst, gate: gate}, err
	}), Options{})
	f.acceptStart()
	awaitProgressGate(t, gate.entered)
	before := observed.updates.Load()
	close(gate.read)
	awaitProgressGate(t, gate.halfway)
	for range 50 {
		child, items, ok := f.engine.ProgressObjects(f.job.ParentID, 1, f.job.ChildID)
		if !ok || child.Assignment != 7 || child.Attempt != 1 || child.Generation != 1 || child.ActiveObjects != 1 || child.SucceededBytes != 0 || child.InFlightBytes != 1<<20 || len(items) != 1 || items[0].TransferredBytes != 1<<20 || items[0].Stage != "TRANSFERRING" {
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
	p := newAttemptProgress(bg, Record{Job: job, Attempt: 4})
	p.initialize(5)
	second := p.begin(job.Items[1])
	first := p.begin(job.Items[0])
	first.bytes.Store(size)
	second.bytes.Store(size / 2)
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
			if tracked && progress.bytes.Load() != int64(b.N)*int64(len(data)) {
				b.Fatal("progress reader lost bytes")
			}
		})
	}
}
