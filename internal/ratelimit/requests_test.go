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
