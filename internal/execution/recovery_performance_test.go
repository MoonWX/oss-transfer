package execution

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type delayedStatStore struct {
	transferstore.Store
	active, maxActive, calls *atomic.Int64
	delay                    time.Duration
	entered                  chan struct{}
	release                  <-chan struct{}
}

func (s delayedStatStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	s.calls.Add(1)
	for prior := s.maxActive.Load(); active > prior; prior = s.maxActive.Load() {
		if s.maxActive.CompareAndSwap(prior, active) {
			break
		}
	}
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return transferstore.Object{}, ctx.Err()
		}
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return transferstore.Object{}, ctx.Err()
		}
	}
	return s.Store.Stat(ctx, key)
}

func TestRecoveryVerificationPerformance(t *testing.T) {
	if os.Getenv("OSS_TRANSFER_PERFORMANCE") == "" {
		t.Skip("set OSS_TRANSFER_PERFORMANCE=1")
	}
	f := newFixture(t, 64)
	f.startEngine(nil, Options{WorkersPerJob: 4})
	f.acceptStart()
	if f.wait().Status != Completed {
		t.Fatal("initial transfer failed")
	}
	var active, maximum, calls atomic.Int64
	factory := f.factory()
	f.engine.factory = StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		source, target, err := factory.Open(ctx, job)
		return source, delayedStatStore{Store: target, active: &active, maxActive: &maximum, calls: &calls, delay: 10 * time.Millisecond}, err
	})
	start := time.Now()
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	record := f.wait()
	elapsed := time.Since(start)
	if record.Status != Completed || calls.Load() != 64 {
		t.Fatalf("recovery skipped checks: status %s calls %d", record.Status, calls.Load())
	}
	f.assertFiles()
	t.Logf("completed_objects=64 simulated_stat_latency=10ms object_limit=4 stat_calls=%d maximum_parallel_checks=%d elapsed=%s content=all_match", calls.Load(), maximum.Load(), elapsed)
}

func TestRecoveryVerificationBoundsAndCancellation(t *testing.T) {
	f := newFixture(t, 12)
	f.startEngine(nil, Options{WorkersPerJob: 4})
	f.acceptStart()
	record := f.wait()
	if record.Status != Completed {
		t.Fatal(record.Status)
	}
	target, err := transferstore.NewLocal(f.dst)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var active, maximum, calls atomic.Int64
	entered := make(chan struct{}, 12)
	blocked := make(chan struct{})
	delayed := delayedStatStore{Store: target, active: &active, maxActive: &maximum, calls: &calls, entered: entered, release: blocked}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	attempt := &activeAttempt{objectsChanged: make(chan struct{})}
	go func() { _, err := f.engine.verifyCompleted(ctx, record, attempt, delayed); done <- err }()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("verification did not fill four object slots")
		}
	}
	if calls.Load() != 4 || maximum.Load() != 4 {
		t.Fatalf("unbounded verification: calls=%d max=%d", calls.Load(), maximum.Load())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verification did not cancel")
	}
	if active.Load() != 0 || attempt.objects != 0 {
		t.Fatal("object slots leaked")
	}
}

type panicStatStore struct{ transferstore.Store }

func (panicStatStore) Stat(context.Context, string) (transferstore.Object, error) {
	panic("broken adapter")
}
func TestRecoveryVerificationContainsAdapterPanic(t *testing.T) {
	_, err := verifyCompletedObject(bg, panicStatStore{}, ItemResult{ID: "1", Target: transferstore.Object{Key: "x"}})
	if err == nil || !strings.Contains(err.Error(), "broken adapter") {
		t.Fatal(err)
	}
}

func TestRecoveryVerificationReportsClosedAdmission(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	f.acceptStart()
	record := f.wait()
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	target, err := transferstore.NewLocal(f.dst)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	_, err = f.engine.verifyCompleted(bg, record, &activeAttempt{objectsChanged: make(chan struct{})}, target)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("closed admission skipped verification: %v", err)
	}
}
