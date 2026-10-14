package agenttransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testSession = Session{NodeID: "node-a", EnrollmentID: "enrollment-1"}

type callResult struct {
	response Response
	err      error
}

func callAsync(b *Broker, ctx context.Context, s Session, r Request) <-chan callResult {
	ch := make(chan callResult, 1)
	go func() { v, e := b.Call(ctx, s, r); ch <- callResult{v, e} }()
	return ch
}
func receive(t *testing.T, ch <-chan callResult) callResult {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("call did not finish")
		return callResult{}
	}
}
func poll(t *testing.T, b *Broker, s Session) Command {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := b.Poll(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func complete(t *testing.T, b *Broker, s Session, c Command) {
	t.Helper()
	if err := b.Complete(s, Result{DeliveryID: c.DeliveryID, RequestID: c.RequestID, Response: Response{Status: 200, Payload: json.RawMessage(`{"ok":true}`)}}); err != nil {
		t.Fatal(err)
	}
}
func idle(t *testing.T, b *Broker) {
	t.Helper()
	s := b.Stats()
	if s.Pending != 0 || s.Queued != 0 || s.PendingBytes != 0 {
		t.Fatalf("resources retained: %+v", s)
	}
}
func awaitPending(t *testing.T, b *Broker, n int) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if b.Stats().Pending == n {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("pending count did not settle")
		}
	}
}
func broker(t *testing.T, o BrokerOptions) *Broker {
	t.Helper()
	b := NewBroker(o)
	t.Cleanup(func() { _ = b.Close() })
	if err := b.Activate(testSession); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBrokerRevocationAndReusedRequestID(t *testing.T) {
	b := broker(t, BrokerOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	oldCall := callAsync(b, ctx, testSession, Request{RequestID: "same-id", Operation: "read"})
	oldCommand := poll(t, b, testSession)
	if err := b.Revoke(testSession); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, oldCall); !errors.Is(got.err, ErrSession) {
		t.Fatal(got.err)
	}
	replacement := Session{NodeID: testSession.NodeID, EnrollmentID: "enrollment-2"}
	if err := b.Activate(replacement); err != nil {
		t.Fatal(err)
	}
	newCall := callAsync(b, ctx, replacement, Request{RequestID: "same-id", Operation: "read"})
	newCommand := poll(t, b, replacement)
	stale := Result{DeliveryID: oldCommand.DeliveryID, RequestID: oldCommand.RequestID, Response: Response{Status: 200}}
	if err := b.Complete(testSession, stale); !errors.Is(err, ErrSession) {
		t.Fatalf("old enrollment accepted: %v", err)
	}
	if err := b.Complete(replacement, stale); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("old delivery accepted: %v", err)
	}
	if err := b.Revoke(testSession); !errors.Is(err, ErrSession) {
		t.Fatal(err)
	}
	complete(t, b, replacement, newCommand)
	if got := receive(t, newCall); got.err != nil {
		t.Fatal(got.err)
	}
	idle(t, b)
	// Reusing the caller ID in the SAME enrollment cannot accept the preceding result.
	again := callAsync(b, ctx, replacement, Request{RequestID: "same-id", Operation: "read"})
	againCommand := poll(t, b, replacement)
	if err := b.Complete(replacement, Result{DeliveryID: newCommand.DeliveryID, RequestID: newCommand.RequestID, Response: Response{Status: 200}}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatal(err)
	}
	complete(t, b, replacement, againCommand)
	if got := receive(t, again); got.err != nil {
		t.Fatal(got.err)
	}
	idle(t, b)
}
func TestBrokerBoundsCancellationAndExpiry(t *testing.T) {
	b := broker(t, BrokerOptions{MaxQueuedPerNode: 1, MaxPending: 2, MaxPayloadBytes: 8, MaxPendingBytes: 2048, MaxCommandDuration: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	first := callAsync(b, ctx, testSession, Request{Operation: "read"})
	awaitPending(t, b, 1)
	if _, err := b.Call(context.Background(), testSession, Request{Operation: "read"}); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), testSession, Request{Operation: "read", Payload: json.RawMessage(`"123456789"`)}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatal(err)
	}
	cancel()
	if got := receive(t, first); !errors.Is(got.err, context.Canceled) {
		t.Fatal(got.err)
	}
	idle(t, b)
	expired := callAsync(b, context.Background(), testSession, Request{Operation: "read"})
	if got := receive(t, expired); !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatal(got.err)
	}
	idle(t, b)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	last := callAsync(b, ctx2, testSession, Request{Operation: "read"})
	awaitPending(t, b, 1)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, last); !errors.Is(got.err, ErrClosed) {
		t.Fatal(got.err)
	}
	idle(t, b)
}
func TestBrokerRejectsOtherNodeAndUndeliveredResult(t *testing.T) {
	b := broker(t, BrokerOptions{})
	other := Session{NodeID: "node-b", EnrollmentID: "other"}
	_ = b.Activate(other)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch := callAsync(b, ctx, testSession, Request{RequestID: "a", Operation: "read"})
	awaitPending(t, b, 1)
	b.mu.Lock()
	id := b.pending["a"].command.DeliveryID
	b.mu.Unlock()
	result := Result{DeliveryID: id, RequestID: "a", Response: Response{Status: 200}}
	if err := b.Complete(testSession, result); !errors.Is(err, ErrNotDelivered) {
		t.Fatal(err)
	}
	c := poll(t, b, testSession)
	if err := b.Complete(other, result); !errors.Is(err, ErrSession) {
		t.Fatal(err)
	}
	complete(t, b, testSession, c)
	_ = receive(t, ch)
	idle(t, b)
}

func httpPair(t *testing.T, b *Broker) (*HTTPTransport, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(HTTPHandler{Broker: b, Authenticate: func(r *http.Request) (Session, error) {
		if r.Header.Get("X-Agent-Key") != "test-key" {
			return Session{}, ErrSession
		}
		return testSession, nil
	}, PollDuration: 100 * time.Millisecond})
	t.Cleanup(server.Close)
	client, err := NewHTTPTransport(server.URL, "test-key", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

type interruptedTransport struct {
	Transport
	pollFailed, submitFailed atomic.Bool
	submitted                chan struct{}
	once                     sync.Once
}

func (t *interruptedTransport) Poll(ctx context.Context) (Command, error) {
	if !t.pollFailed.Swap(true) {
		return Command{}, io.ErrUnexpectedEOF
	}
	return t.Transport.Poll(ctx)
}
func (t *interruptedTransport) Submit(ctx context.Context, r Result) error {
	err := t.Transport.Submit(ctx, r)
	if err != nil {
		return err
	}
	if !t.submitFailed.Swap(true) {
		return io.ErrUnexpectedEOF
	}
	t.once.Do(func() { close(t.submitted) })
	return nil
}
func startRunner(t *testing.T, r *Runner) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("runner failed to cancel")
		}
	})
	return cancel, done
}
func TestHTTPRoundTripReconnectAndMutationReplay(t *testing.T) {
	b := broker(t, BrokerOptions{})
	client, _ := httpPair(t, b)
	flaky := &interruptedTransport{Transport: client, submitted: make(chan struct{})}
	var writes atomic.Int32
	r := NewRunner(flaky, ExecutorFunc(func(ctx context.Context, c Command) (Response, error) {
		writes.Add(1)
		return Response{Status: 200, Payload: json.RawMessage(`{"written":true}`)}, nil
	}), NewMemoryJournal(10), RunnerOptions{ReconnectDelay: time.Millisecond})
	startRunner(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := Request{Operation: "write", IdempotencyKey: "task-1", Mutating: true, Payload: json.RawMessage(`{"key":"a"}`)}
	result, err := b.Call(ctx, testSession, request)
	if err != nil || result.Status != 200 {
		t.Fatalf("%+v %v", result, err)
	}
	select {
	case <-flaky.submitted:
	case <-ctx.Done():
		t.Fatal("lost result was not resubmitted")
	}
	result, err = b.Call(ctx, testSession, request)
	if err != nil || result.Status != 200 || writes.Load() != 1 {
		t.Fatalf("replay executed again: %+v %v writes=%d", result, err, writes.Load())
	}
	request.Payload = json.RawMessage(`{"key":"b"}`)
	result, err = b.Call(ctx, testSession, request)
	if err != nil || result.Status != 409 || writes.Load() != 1 {
		t.Fatalf("idempotency conflict not rejected: %+v %v", result, err)
	}
	idle(t, b)
}
func TestRunnerCancellationPreservesUnknownMutation(t *testing.T) {
	b := broker(t, BrokerOptions{DeliveryLease: 25 * time.Millisecond})
	client, _ := httpPair(t, b)
	journal := NewMemoryJournal(10)
	entered := make(chan struct{})
	cancelObserved := make(chan struct{})
	var executions atomic.Int32
	runner := NewRunner(client, ExecutorFunc(func(ctx context.Context, c Command) (Response, error) {
		executions.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelObserved)
		return Response{}, ctx.Err()
	}), journal, RunnerOptions{})
	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	request := Request{Operation: "write", Mutating: true, IdempotencyKey: "x"}
	call := callAsync(b, callCtx, testSession, request)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	cancelRun()
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("executor context not cancelled")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	cancelCall()
	_ = receive(t, call)
	idle(t, b)
	replacement := NewRunner(client, ExecutorFunc(func(context.Context, Command) (Response, error) { executions.Add(1); return Response{Status: 200}, nil }), journal, RunnerOptions{})
	startRunner(t, replacement)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := b.Call(ctx, testSession, request)
	if err != nil || response.Status != 409 || !strings.Contains(string(response.Payload), "outcome_unknown") || executions.Load() != 1 {
		t.Fatalf("uncertain mutation re-executed: %+v %v count=%d", response, err, executions.Load())
	}
	idle(t, b)
}
func TestHTTPAuthenticationAndRedirectProtection(t *testing.T) {
	b := broker(t, BrokerOptions{})
	_, server := httpPair(t, b)
	bad, _ := NewHTTPTransport(server.URL, "wrong", nil, 0)
	if _, err := bad.Poll(context.Background()); err == nil {
		t.Fatal("unauthenticated poll succeeded")
	}
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, _ := NewHTTPTransport(redirect.URL, "secret", nil, 0)
	if _, err := client.Poll(context.Background()); err == nil || leaked.Load() {
		t.Fatal("credential followed redirect")
	}
}
func TestJournalBoundedAndNoMutationWithoutJournal(t *testing.T) {
	journal := NewMemoryJournal(1)
	ctx := context.Background()
	claim, err := journal.Begin(ctx, "key", "fingerprint")
	if err != nil || !claim.Acquired {
		t.Fatal(err)
	}
	if _, err = journal.Begin(ctx, "second", "x"); !errors.Is(err, ErrJournalFull) {
		t.Fatal(err)
	}
	if _, err = journal.Begin(ctx, "key", "changed"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal(err)
	}
	journal.maxBytes = 2
	if err = journal.Complete(ctx, "key", "fingerprint", Response{Status: 200, Payload: json.RawMessage(`"large"`)}); !errors.Is(err, ErrJournalFull) {
		t.Fatal(err)
	}
	var called bool
	r := NewRunner(nil, ExecutorFunc(func(context.Context, Command) (Response, error) { called = true; return Response{Status: 200}, nil }), nil, RunnerOptions{})
	response := r.runCommand(ctx, Command{Session: testSession, DeliveryID: "d", Request: Request{RequestID: "r", Operation: "write", IdempotencyKey: "key", Mutating: true}, DeadlineUnixMilli: time.Now().Add(time.Second).UnixMilli()})
	if called || response.Status != 503 || !strings.Contains(string(response.Payload), "journal_required") {
		t.Fatal("mutation ran without a journal")
	}
}

func TestHTTPReEnrollmentInvalidatesLongPoll(t *testing.T) {
	b := broker(t, BrokerOptions{})
	replacement := Session{NodeID: testSession.NodeID, EnrollmentID: "enrollment-new"}
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(HTTPHandler{Broker: b, Authenticate: func(r *http.Request) (Session, error) {
		switch r.Header.Get("X-Agent-Key") {
		case "old":
			once.Do(func() { close(entered) })
			return testSession, nil
		case "new":
			return replacement, nil
		}
		return Session{}, ErrSession
	}})
	defer server.Close()
	oldClient, _ := NewHTTPTransport(server.URL, "old", nil, 0)
	newClient, _ := NewHTTPTransport(server.URL, "new", nil, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	oldPoll := make(chan error, 1)
	go func() { _, err := oldClient.Poll(ctx); oldPoll <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("long poll never arrived")
	}
	if err := b.Activate(replacement); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-oldPoll:
		if err == nil {
			t.Fatal("old long poll survived enrollment replacement")
		}
	case <-ctx.Done():
		t.Fatal("revocation did not wake long poll")
	}
	call := callAsync(b, ctx, replacement, Request{Operation: "read"})
	c, err := newClient.Poll(ctx)
	if err != nil || c.Session != replacement {
		t.Fatalf("%+v %v", c, err)
	}
	result := Result{DeliveryID: c.DeliveryID, RequestID: c.RequestID, Response: Response{Status: 200}}
	if err = oldClient.Submit(ctx, result); err == nil {
		t.Fatal("old credential completed replacement command")
	}
	if err = newClient.Submit(ctx, result); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, call); got.err != nil {
		t.Fatal(got.err)
	}
	idle(t, b)
}
func TestBrokerGlobalPendingAndPayloadBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options BrokerOptions
	}{
		{"pending", BrokerOptions{MaxPending: 1}},
		{"payload", BrokerOptions{MaxPending: 10, MaxPendingBytes: 1026}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := broker(t, tc.options)
			other := Session{NodeID: "node-b", EnrollmentID: "other"}
			_ = b.Activate(other)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			call := callAsync(b, ctx, testSession, Request{Operation: "read", Payload: json.RawMessage(`{}`)})
			_ = poll(t, b, testSession)
			if _, err := b.Call(ctx, other, Request{Operation: "read"}); !errors.Is(err, ErrQueueFull) {
				t.Fatalf("global budget ignored: %v", err)
			}
			cancel()
			_ = receive(t, call)
			idle(t, b)
		})
	}
}

func TestRevocationCancelsAgentExecution(t *testing.T) {
	b := broker(t, BrokerOptions{})
	client, _ := httpPair(t, b)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	r := NewRunner(client, ExecutorFunc(func(ctx context.Context, c Command) (Response, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return Response{}, ctx.Err()
	}), NewMemoryJournal(10), RunnerOptions{Concurrency: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	call := callAsync(b, ctx, testSession, Request{Operation: "write", Mutating: true, IdempotencyKey: "revoked"})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("executor did not start")
	}
	if err := b.Revoke(testSession); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, call); !errors.Is(got.err, ErrSession) {
		t.Fatal(got.err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("revoked session did not cancel executor")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrSession) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("revoked runner did not exit")
	}
	idle(t, b)
}
