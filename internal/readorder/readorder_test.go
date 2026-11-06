package readorder

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// tokens models a limiter's wait for d: it gives way when preempted.
func tokens(d time.Duration) func(<-chan struct{}) error {
	return func(yield <-chan struct{}) error {
		select {
		case <-yield:
			return ErrPreempted
		case <-time.After(d):
			return nil
		}
	}
}

func free(q *Queue) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.holder == nil && len(q.waiting) == 0
}

func TestZeroTurnServesAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed := errors.New("reserve failed")
	calls := 0
	err := From(context.Background()).Serve(ctx, func(yield <-chan struct{}) error {
		calls++
		if yield != nil {
			t.Error("the zero turn can be preempted")
		}
		return failed
	})
	if calls != 1 || !errors.Is(err, failed) {
		t.Fatalf("%d calls, %v", calls, err)
	}
}

// A released queue goes to the lowest waiting rank, whatever order the
// waiters arrived in, and is free again once every read is served.
func TestReleasedQueueGoesToLowestWaitingRank(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		release := make(chan struct{})
		go func() {
			_ = From(With(context.Background(), queue, 0)).Serve(context.Background(), func(yield <-chan struct{}) error {
				<-release
				return nil
			})
		}()
		synctest.Wait()
		served := make(chan int64, 4)
		for _, rank := range []int64{9, 2, 5, 3} {
			go func() {
				_ = From(With(context.Background(), queue, rank)).Serve(context.Background(), func(<-chan struct{}) error {
					served <- rank
					return nil
				})
			}()
			synctest.Wait() // arrival order: 9, 2, 5, 3
		}
		close(release)
		synctest.Wait()
		close(served)
		var order []int64
		for rank := range served {
			order = append(order, rank)
		}
		if !slices.Equal(order, []int64{2, 3, 5, 9}) {
			t.Fatalf("served %v, want lowest rank first", order)
		}
		if !free(queue) {
			t.Fatal("queue not free")
		}
	})
}

// A lower rank that starts waiting takes over from a holder still waiting
// for tokens, which then waits for the queue again.
func TestLowerRankPreemptsHolderWaitingForTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		served := make(chan int64, 2)
		attempts := 0
		higher := make(chan struct{})
		go func() {
			defer close(higher)
			_ = From(With(context.Background(), queue, 5)).Serve(context.Background(), func(yield <-chan struct{}) error {
				attempts++
				if err := tokens(time.Second)(yield); err != nil {
					return err
				}
				served <- 5
				return nil
			})
		}()
		synctest.Wait()
		_ = From(With(context.Background(), queue, 1)).Serve(context.Background(), func(yield <-chan struct{}) error {
			served <- 1
			return nil
		})
		<-higher
		close(served)
		var order []int64
		for rank := range served {
			order = append(order, rank)
		}
		if !slices.Equal(order, []int64{1, 5}) || attempts != 2 {
			t.Fatalf("served %v after %d attempts of rank 5", order, attempts)
		}
		if !free(queue) {
			t.Fatal("queue not free")
		}
	})
}

// Ranges that a lower rank keeps passing over, even one preempted every time
// it gets the queue (and so queued again behind a later range), are served
// once each has waited MaxPassedOver, longest waiting first, by a grant that
// cannot be preempted.
func TestPassedOverRangesAreServedWithinBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		ctx, stop := context.WithCancel(context.Background())
		lowest := make(chan struct{})
		go func() {
			defer close(lowest)
			turn := From(With(ctx, queue, 0))
			for turn.Serve(ctx, tokens(50*time.Millisecond)) == nil {
			}
		}()
		synctest.Wait()
		serve := func(rank int64, waited chan<- time.Duration) {
			start := time.Now()
			if err := From(With(context.Background(), queue, rank)).Serve(context.Background(), tokens(50*time.Millisecond)); err != nil {
				t.Error(err)
			}
			waited <- time.Since(start)
		}
		first, second := make(chan time.Duration, 1), make(chan time.Duration, 1)
		go serve(1, first)
		time.Sleep(time.Second)
		go serve(2, second)
		waits := []time.Duration{<-first, <-second}
		stop()
		<-lowest
		for i, waited := range waits {
			if waited < MaxPassedOver || waited > MaxPassedOver+100*time.Millisecond {
				t.Errorf("passed-over rank %d served after %v, want just after %v", i+1, waited, MaxPassedOver)
			}
		}
		synctest.Wait()
		if !free(queue) {
			t.Fatal("queue not free")
		}
	})
}

// A waiter that is cancelled leaves the queue with its cause, and the queue
// still reaches every other waiter.
func TestCancelledWaiterLeavesQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		release := make(chan struct{})
		go func() {
			_ = From(With(context.Background(), queue, 0)).Serve(context.Background(), func(<-chan struct{}) error {
				<-release
				return nil
			})
		}()
		synctest.Wait()
		cause := errors.New("range abandoned")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancelled := make(chan error, 1)
		go func() {
			cancelled <- From(With(ctx, queue, 1)).Serve(ctx, func(<-chan struct{}) error {
				t.Error("cancelled waiter was served")
				return nil
			})
		}()
		served := make(chan struct{})
		go func() {
			_ = From(With(context.Background(), queue, 2)).Serve(context.Background(), func(<-chan struct{}) error {
				close(served)
				return nil
			})
		}()
		synctest.Wait()
		cancel(cause)
		if err := <-cancelled; !errors.Is(err, cause) {
			t.Fatalf("cancelled wait returned %v", err)
		}
		close(release)
		<-served
		synctest.Wait()
		if !free(queue) {
			t.Fatal("queue not free")
		}
	})
}

// A waiter woken by its cancellation just after the queue was handed to it
// passes the queue on instead of keeping it.
func TestWaiterCancelledAsQueueIsHandedOverPassesItOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := NewQueue()
		if _, err := queue.acquire(context.Background(), &waiter{rank: 0, since: time.Now()}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		first := make(chan error, 1)
		go func() {
			first <- From(With(ctx, queue, 1)).Serve(ctx, func(<-chan struct{}) error {
				t.Error("cancelled waiter was served")
				return nil
			})
		}()
		served := make(chan struct{})
		go func() {
			_ = From(With(context.Background(), queue, 2)).Serve(context.Background(), func(<-chan struct{}) error {
				close(served)
				return nil
			})
		}()
		synctest.Wait()
		// The cancellation wakes rank 1 first; the holder's release then hands
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
		if !free(queue) {
			t.Fatal("queue not free")
		}
	})
}
