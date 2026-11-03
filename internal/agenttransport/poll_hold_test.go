package agenttransport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pollRecorder serves the real handler with the default 25 s hold and records
// each GET poll's query, so tests observe holding through Broker.Stats instead
// of elapsed time.
type pollRecorder struct {
	handler HTTPHandler
	mu      sync.Mutex
	queries []string
	active  atomic.Int32
}

func (p *pollRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == PollPath && r.Method == http.MethodGet {
		p.mu.Lock()
		p.queries = append(p.queries, r.URL.RawQuery)
		p.mu.Unlock()
		p.active.Add(1)
		defer p.active.Add(-1)
	}
	p.handler.ServeHTTP(w, r)
}
func (p *pollRecorder) polls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

func heldPair(t *testing.T, b *Broker) (*HTTPTransport, *httptest.Server, *pollRecorder) {
	t.Helper()
	recorder := &pollRecorder{handler: HTTPHandler{Broker: b, Authenticate: func(r *http.Request) (Session, error) {
		if r.Header.Get("X-Agent-Key") != "test-key" {
			return Session{}, ErrSession
		}
		return testSession, nil
	}}}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	client, err := NewHTTPTransport(server.URL, "test-key", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return client, server, recorder
}

type pollResult struct {
	command Command
	err     error
}

func pollAsync(ctx context.Context, client Transport) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() { c, err := client.Poll(ctx); ch <- pollResult{c, err} }()
	return ch
}

// until spins on observable state; the deadline is a hang guard only.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFirstHTTPPollIsNotHeldAndLaterPollsAre(t *testing.T) {
	b := broker(t, BrokerOptions{})
	client, _, recorder := heldPair(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := pollAsync(ctx, client)
	// A held poll stays counted until it returns, so observing it is decisive.
	var result pollResult
	until(t, "first poll", func() bool {
		if b.Stats().Waiting > 0 {
			t.Fatal("first poll of a fresh transport was held")
		}
		select {
		case result = <-first:
			return true
		default:
			return false
		}
	})
	if !errors.Is(result.err, ErrNoCommand) {
		t.Fatal(result.err)
	}
	second := pollAsync(ctx, client)
	until(t, "second poll to be held", func() bool {
		select {
		case r := <-second:
			t.Fatalf("second poll was not held: %+v %v", r.command, r.err)
		default:
		}
		return b.Stats().Waiting == 1
	})
	call := callAsync(b, ctx, testSession, Request{Operation: "read"})
	r := <-second
	if r.err != nil || r.command.Operation != "read" {
		t.Fatalf("held poll did not deliver: %+v %v", r.command, r.err)
	}
	if err := client.Submit(ctx, Result{DeliveryID: r.command.DeliveryID, RequestID: r.command.RequestID, Response: Response{Status: 200}}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, call); got.err != nil || got.response.Status != 200 {
		t.Fatalf("%+v %v", got.response, got.err)
	}
	if got := recorder.polls(); !slices.Equal(got, []string{pollNoWait, ""}) {
		t.Fatalf("poll queries=%q", got)
	}
	idle(t, b)
}

func TestReconnectAfterFirstPollKeepsHolding(t *testing.T) {
	b := broker(t, BrokerOptions{})
	client, server, recorder := heldPair(t, b)
	runner := NewRunner(client, ExecutorFunc(func(context.Context, Command) (Response, error) { return Response{Status: 200}, nil }), NewMemoryJournal(10), RunnerOptions{ReconnectDelay: time.Millisecond})
	startRunner(t, runner)
	until(t, "held poll", func() bool { return b.Stats().Waiting == 1 && len(recorder.polls()) == 2 })
	server.CloseClientConnections()
	// Only the replacement request may be in flight; an immediate one would
	// return at once and never be observed held.
	until(t, "held poll after reconnect", func() bool {
		return len(recorder.polls()) >= 3 && recorder.active.Load() == 1 && b.Stats().Waiting == 1
	})
	// The HTTP client may transparently retry the interrupted GET; any retry
	// must still be held.
	if got := recorder.polls(); got[0] != pollNoWait || slices.Contains(got[1:], pollNoWait) {
		t.Fatalf("reconnect skipped the hold: poll queries=%q", got)
	}
}

func TestUnauthenticatedFirstPollStaysNoWait(t *testing.T) {
	b := broker(t, BrokerOptions{})
	_, server, recorder := heldPair(t, b)
	bad, err := NewHTTPTransport(server.URL, "wrong", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := bad.Poll(context.Background()); !errors.Is(err, ErrSession) {
			t.Fatal(err)
		}
	}
	// Only an authenticated answer ends the no-wait poll; readiness never fired.
	if got := recorder.polls(); !slices.Equal(got, []string{pollNoWait, pollNoWait}) {
		t.Fatalf("poll queries=%q", got)
	}
}

func TestQueuedCommandDeliveredOnceByFirstPoll(t *testing.T) {
	b := broker(t, BrokerOptions{})
	client, _, recorder := heldPair(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // hang guard only
	defer cancel()
	call := callAsync(b, ctx, testSession, Request{Operation: "write", Mutating: true, IdempotencyKey: "queued"})
	until(t, "queued command", func() bool { return b.Stats().Queued == 1 })
	var executions atomic.Int32
	var unconfirmed atomic.Bool
	runner := NewRunner(client, ExecutorFunc(func(_ context.Context, c Command) (Response, error) {
		b.mu.Lock()
		p := b.pending[c.RequestID]
		if p == nil || !p.confirmed || p.command.DeliveryID != c.DeliveryID {
			unconfirmed.Store(true)
		}
		b.mu.Unlock()
		executions.Add(1)
		return Response{Status: 200}, nil
	}), NewMemoryJournal(10), RunnerOptions{})
	startRunner(t, runner)
	if got := receive(t, call); got.err != nil || got.response.Status != 200 {
		t.Fatalf("%+v %v", got.response, got.err)
	}
	until(t, "held poll", func() bool { return b.Stats().Waiting == 1 })
	if executions.Load() != 1 || unconfirmed.Load() {
		t.Fatalf("executions=%d unconfirmed=%t", executions.Load(), unconfirmed.Load())
	}
	if got := recorder.polls(); !slices.Equal(got, []string{pollNoWait, ""}) {
		t.Fatalf("poll queries=%q", got)
	}
	idle(t, b)
}

func TestNoWaitPollKeepsDeliveryLease(t *testing.T) {
	b := broker(t, BrokerOptions{DeliveryLease: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := callAsync(b, ctx, testSession, Request{Operation: "read"})
	until(t, "queued command", func() bool { return b.Stats().Queued == 1 })
	// Background context: a held poll would block, so a return proves no hold.
	leased, err := b.poll(context.Background(), testSession, true, false)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	leaseEnd := b.pending[leased.RequestID].leaseEnd
	b.mu.Unlock()
	if _, err := b.poll(context.Background(), testSession, true, false); !errors.Is(err, ErrNoCommand) {
		t.Fatal("leased command redelivered:", err)
	}
	b.mu.Lock()
	p := b.pending[leased.RequestID]
	unchanged := p != nil && p.command.DeliveryID == leased.DeliveryID && p.leaseEnd.Equal(leaseEnd) && !p.confirmed
	b.mu.Unlock()
	if !unchanged {
		t.Fatal("no-wait poll changed the delivery lease")
	}
	if err := b.Confirm(testSession, Receipt{RequestID: leased.RequestID, DeliveryID: leased.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	complete(t, b, testSession, leased)
	if got := receive(t, call); got.err != nil {
		t.Fatal(got.err)
	}
	idle(t, b)
}
