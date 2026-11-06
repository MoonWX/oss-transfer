package readorder

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
)

func TestZeroTurnNeverWaits(t *testing.T) {
	turn := From(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	turn.Done()
}

// A released queue goes to the lowest waiting rank, whatever order the
// waiters arrived in, and is free again once every turn is done.
func TestReleasedQueueGoesToLowestWaitingRank(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		holder := From(With(context.Background(), queue, 7))
		if err := holder.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		served := make(chan int64, 4)
		for _, rank := range []int64{9, 2, 5, 3} {
			go func() {
				turn := From(With(context.Background(), queue, rank))
				if err := turn.Wait(context.Background()); err != nil {
					t.Error(err)
					return
				}
				served <- rank
				turn.Done()
			}()
			synctest.Wait() // arrival order: 9, 2, 5, 3
		}
		holder.Done()
		synctest.Wait()
		close(served)
		var order []int64
		for rank := range served {
			order = append(order, rank)
		}
		if !slices.Equal(order, []int64{2, 3, 5, 9}) {
			t.Fatalf("served %v, want lowest rank first", order)
		}
		if queue.held || len(queue.waiting) != 0 {
			t.Fatalf("queue not free: held=%v waiting=%d", queue.held, len(queue.waiting))
		}
	})
}

// A waiter that is cancelled leaves the queue with its cause, and the queue
// still reaches every other waiter.
func TestCancelledWaiterLeavesQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		holder := From(With(context.Background(), queue, 0))
		if err := holder.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("range abandoned")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancelled := make(chan error, 1)
		go func() { cancelled <- From(With(ctx, queue, 1)).Wait(ctx) }()
		served := make(chan struct{})
		go func() {
			turn := From(With(context.Background(), queue, 2))
			if err := turn.Wait(context.Background()); err == nil {
				close(served)
				turn.Done()
			}
		}()
		synctest.Wait()
		cancel(cause)
		if err := <-cancelled; !errors.Is(err, cause) {
			t.Fatalf("cancelled wait returned %v", err)
		}
		holder.Done()
		<-served
		synctest.Wait()
		if queue.held || len(queue.waiting) != 0 {
			t.Fatalf("queue not free: held=%v waiting=%d", queue.held, len(queue.waiting))
		}
	})
}

// A waiter woken by its cancellation just after the queue was handed to it
// passes the queue on instead of keeping it.
func TestWaiterCancelledAsQueueIsHandedOverPassesItOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		holder := From(With(context.Background(), queue, 0))
		if err := holder.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		first := make(chan error, 1)
		go func() { first <- From(With(ctx, queue, 1)).Wait(ctx) }()
		served := make(chan struct{})
		go func() {
			turn := From(With(context.Background(), queue, 2))
			if err := turn.Wait(context.Background()); err == nil {
				turn.Done()
				close(served)
			}
		}()
		synctest.Wait()
		// The cancellation wakes rank 1 first, and the holder's release hands
		// it the queue while it waits for the lock to leave.
		queue.mu.Lock()
		cancel()
		queue.handOff()
		queue.mu.Unlock()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait returned %v", err)
		}
		<-served
		synctest.Wait()
		if queue.held || len(queue.waiting) != 0 {
			t.Fatalf("queue not free: held=%v waiting=%d", queue.held, len(queue.waiting))
		}
	})
}
