package execution

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func waitAdmission(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not enter the newly available slot")
		return ""
	}
}

func noAdmission(t *testing.T, entered <-chan string) {
	t.Helper()
	select {
	case id := <-entered:
		t.Fatalf("transfer %s exceeded the reduced concurrency", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestObjectConcurrencyChangesWithinRunningChild(t *testing.T) {
	f := newFixture(t, 6)
	entered := make(chan string, 6)
	gates := map[string]chan struct{}{}
	for _, item := range f.job.Items {
		gates[item.ID] = make(chan struct{})
	}
	f.startEngine(nil, Options{WorkersPerJob: 1, DecorateReader: func(ctx context.Context, _ Job, item Item, input io.ReadCloser) (io.ReadCloser, error) {
		entered <- item.ID
		select {
		case <-gates[item.ID]:
			return input, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	f.acceptStart()
	first := waitAdmission(t, entered)
	noAdmission(t, entered)
	if err := f.engine.SetConcurrency(8, 3); err != nil {
		t.Fatal(err)
	}
	second, third := waitAdmission(t, entered), waitAdmission(t, entered)
	if err := f.engine.SetConcurrency(8, 1); err != nil {
		t.Fatal(err)
	}
	close(gates[first])
	noAdmission(t, entered) // Two old transfers still occupy this child's slots.
	close(gates[second])
	noAdmission(t, entered) // The remaining transfer is not cancelled or replaced.
	close(gates[third])
	for range 3 {
		close(gates[waitAdmission(t, entered)])
	}
	if result := f.wait(); result.Status != Completed || result.Counters.Succeeded != 6 || result.Counters.Failed != 0 {
		t.Fatal(result)
	}
	f.assertFiles()
}

func TestChildConcurrencyChangesWakeAndDrainWithoutCancelling(t *testing.T) {
	f := newFixture(t, 1)
	entered := make(chan string, 3)
	gates := map[string]chan struct{}{}
	jobs := make([]Job, 3)
	for i := range jobs {
		job := f.job
		job.ChildID, job.Number = fmt.Sprintf("224-%d", i+1), i+1
		job.Items = append([]Item(nil), f.job.Items...)
		job.Items[0].DestinationKey = fmt.Sprintf("child-%d.bin", i+1)
		jobs[i], gates[job.ChildID] = job, make(chan struct{})
	}
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		entered <- job.ChildID
		select {
		case <-gates[job.ChildID]:
			return f.factory().Open(ctx, job)
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}), Options{MaxConcurrentJobs: 1})
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	results := make(chan error, 3)
	for _, job := range jobs {
		if _, err := f.engine.Accept(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	go func() { results <- f.engine.Start(ctx, jobs[0].ChildID, 1) }()
	first := waitAdmission(t, entered)
	go func() { results <- f.engine.Start(ctx, jobs[1].ChildID, 1) }()
	noAdmission(t, entered)
	if err := f.engine.SetConcurrency(2, 4); err != nil {
		t.Fatal(err)
	}
	second := waitAdmission(t, entered)
	if err := f.engine.SetConcurrency(1, 4); err != nil {
		t.Fatal(err)
	}
	go func() { results <- f.engine.Start(ctx, jobs[2].ChildID, 1) }()
	close(gates[first])
	noAdmission(t, entered)
	close(gates[second])
	close(gates[waitAdmission(t, entered)])
	for range jobs {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, job := range jobs {
		result, err := f.engine.Wait(ctx, job.ChildID)
		if err != nil || result.Status != Completed || result.Counters.Succeeded != 1 || result.Attempt != 1 {
			t.Fatal(result, err)
		}
	}
}

func TestDefaultConcurrencyIsEightChildrenAndFourObjects(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{})
	_, children := f.engine.Concurrency()
	if children != 8 || f.engine.ObjectConcurrency() != 4 {
		t.Fatal(children, f.engine.ObjectConcurrency())
	}
}

func TestCapacityChangedNotifiesCompletionAndRaisedLimit(t *testing.T) {
	f := newFixture(t, 1)
	f.startEngine(nil, Options{MaxConcurrentJobs: 1})
	completed := f.engine.CapacityChanged()
	f.acceptStart()
	if record := f.wait(); record.Status != Completed {
		t.Fatal(record.Status)
	}
	select {
	case <-completed:
	default:
		t.Fatal("completion did not wake admission")
	}
	raised := f.engine.CapacityChanged()
	select {
	case <-raised:
		t.Fatal("new capacity subscription already closed")
	default:
	}
	if err := f.engine.SetConcurrency(8, 4); err != nil {
		t.Fatal(err)
	}
	select {
	case <-raised:
	default:
		t.Fatal("raised concurrency did not wake admission")
	}
}
