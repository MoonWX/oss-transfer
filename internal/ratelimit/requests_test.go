package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestRequestJointBudgetDoesNotReserveWhileNodeBlocked(t *testing.T) {
	f := newFixture(t)
	l := f.limiter
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 1, PutQPS: 1.0 / 3600}); err != nil {
		t.Fatal(err)
	}
	// Seed an already due parent permit. Far-away deadlines make the test
	// independent of scheduler speed without sleeping or weakening a QPS cap.
	due := time.Now().Add(-time.Hour)
	l.parents[1].requestNext[2] = due
	nodeDeadline := time.Now().Add(time.Hour)
	for i := 0; i < 64; i++ {
		delay, _, err := l.TryRequest(1, 1, "put", nodeDeadline)
		if err != nil || delay <= 0 {
			t.Fatalf("node-blocked attempt admitted: delay=%s err=%v", delay, err)
		}
	}
	if got := l.parents[1].requestNext[2]; !got.Equal(due) {
		t.Fatalf("waiting for node consumed or reserved parent permits: %s", got)
	}

	// Releasing the node deadline exposes one parent permit to all waiting
	// children. Concurrent retries may consume exactly one, never a backlog.
	start := make(chan struct{})
	results := make(chan time.Duration, 64)
	errors := make(chan error, 64)
	var group sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			delay, _, err := l.TryRequest(1, 1, "put", time.Time{})
			results <- delay
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	admitted := 0
	for delay := range results {
		if delay <= 0 {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("joint budget admitted %d concurrent attempts, want exactly one", admitted)
	}
}

// A QPS change within a generation keeps the spacing anchored at the last
// admitted request: a raise is due one new interval after it (not one new
// interval after the change), a reduction one new interval after it.
func TestRequestQPSChangeReanchorsNextPermit(t *testing.T) {
	f := newFixture(t)
	l := f.limiter
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 1, ListQPS: 1}); err != nil {
		t.Fatal(err)
	}
	// Far-away anchors keep the test independent of scheduler speed.
	last := time.Now().Add(-time.Hour)
	l.parents[1].requestNext[0] = last.Add(time.Second)
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 2, ListQPS: 4}); err != nil {
		t.Fatal(err)
	}
	if got := l.parents[1].requestNext[0]; !got.Equal(last.Add(250 * time.Millisecond)) {
		t.Fatal("raise not re-anchored at the last permit", got.Sub(last))
	}
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 3, ListQPS: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := l.parents[1].requestNext[0]; !got.Equal(last.Add(2 * time.Second)) {
		t.Fatal("reduction not spaced from the last permit", got.Sub(last))
	}
	// The re-anchored permit is already due: the next request is admitted.
	if delay, _, err := l.TryRequest(1, 1, "list", time.Time{}); err != nil || delay != 0 {
		t.Fatal("a raise must not wait a whole new interval", delay, err)
	}
	// Removing the limit, or a limit without a previous permit, starts over.
	if err := l.ApplyParent(bg, ParentLimit{ParentID: 1, Generation: 1, Revision: 4}); err != nil {
		t.Fatal(err)
	}
	if !l.parents[1].requestNext[0].IsZero() {
		t.Fatal("unlimited keeps a permit time")
	}
	if got := reanchorRequest(time.Time{}, 1, 2); !got.IsZero() {
		t.Fatal("no previous permit must stay unset", got)
	}
	if got := reanchorRequest(last, minimumTestQPS, 1); !got.IsZero() {
		t.Fatal("an extreme interval must restart its spacing", got)
	}
}

const minimumTestQPS = 1e-12
