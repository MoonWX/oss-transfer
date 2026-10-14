package agenttransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// loseResponse drops bytes only after the actual server handled the request.
// Unlike returning an error before RoundTrip, this exercises uncertain delivery.
type loseResponse struct {
	method string
	once   atomic.Bool
	lost   chan struct{}
}

func (rt *loseResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	expectedStatus := http.StatusOK
	if rt.method == http.MethodPost {
		expectedStatus = http.StatusNoContent
	}
	if r.URL.Path == PollPath && r.Method == rt.method && response.StatusCode == expectedStatus && !rt.once.Swap(true) {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		close(rt.lost)
		return nil, io.ErrUnexpectedEOF
	}
	return response, nil
}

func TestLostPollResponseRedeliveredBeforeExecution(t *testing.T) {
	b := broker(t, BrokerOptions{DeliveryLease: 25 * time.Millisecond})
	_, server := httpPair(t, b)
	rt := &loseResponse{method: http.MethodGet, lost: make(chan struct{})}
	client, err := NewHTTPTransport(server.URL, "test-key", rt, 0)
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	runner := NewRunner(client, ExecutorFunc(func(context.Context, Command) (Response, error) {
		executions.Add(1)
		return Response{Status: 200}, nil
	}), NewMemoryJournal(10), RunnerOptions{ReconnectDelay: time.Millisecond})
	startRunner(t, runner)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := b.Call(ctx, testSession, Request{Operation: "write", Mutating: true, IdempotencyKey: "lost-poll"})
	if err != nil || response.Status != 200 || executions.Load() != 1 || !rt.once.Load() {
		t.Fatalf("lost command was not recovered exactly once: response=%+v error=%v executions=%d", response, err, executions.Load())
	}
	idle(t, b)
}

func TestLostReceiptResponseReleasesOnCallerCancel(t *testing.T) {
	b := broker(t, BrokerOptions{DeliveryLease: 25 * time.Millisecond})
	_, server := httpPair(t, b)
	rt := &loseResponse{method: http.MethodPost, lost: make(chan struct{})}
	client, err := NewHTTPTransport(server.URL, "test-key", rt, 0)
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	runner := NewRunner(client, ExecutorFunc(func(context.Context, Command) (Response, error) {
		executions.Add(1)
		return Response{Status: 200}, nil
	}), NewMemoryJournal(10), RunnerOptions{ReconnectDelay: time.Millisecond})
	startRunner(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := Request{Operation: "write", Mutating: true, IdempotencyKey: "lost-receipt"}
	first := callAsync(b, ctx, testSession, request)
	select {
	case <-rt.lost:
	case <-time.After(time.Second):
		t.Fatal("receipt was not sent")
	}
	if executions.Load() != 0 {
		t.Fatal("executed without confirmed receipt response")
	}
	cancel()
	if result := receive(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatal(result.err)
	}
	idle(t, b)
	// The original command never ran; retrying the business idempotency key can
	// acquire its journal claim. A started mutation instead retains unknown.
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	response, err := b.Call(retryCtx, testSession, request)
	if err != nil || response.Status != 200 || executions.Load() != 1 {
		t.Fatalf("retry after lost receipt: response=%+v error=%v executions=%d", response, err, executions.Load())
	}
	idle(t, b)
}

func TestLeasedDeliveryRejectsStaleConfirmationAndResult(t *testing.T) {
	b := broker(t, BrokerOptions{DeliveryLease: 10 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	call := callAsync(b, ctx, testSession, Request{Operation: "read"})
	first, err := b.poll(ctx, testSession, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Complete(testSession, Result{RequestID: first.RequestID, DeliveryID: first.DeliveryID, Response: Response{Status: 200}}); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("unconfirmed result accepted: %v", err)
	}
	// The broker's lease timer wakes this blocked poll; no sleeps establish order.
	second, err := b.poll(ctx, testSession, true)
	if err != nil || second.RequestID != first.RequestID || second.DeliveryID == first.DeliveryID {
		t.Fatalf("redelivery failed: first=%+v second=%+v error=%v", first, second, err)
	}
	if err = b.Confirm(testSession, Receipt{RequestID: first.RequestID, DeliveryID: first.DeliveryID}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("stale confirmation accepted: %v", err)
	}
	if err = b.Complete(testSession, Result{RequestID: first.RequestID, DeliveryID: first.DeliveryID, Response: Response{Status: 200}}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("stale result accepted: %v", err)
	}
	for range 2 {
		if err = b.Confirm(testSession, Receipt{RequestID: second.RequestID, DeliveryID: second.DeliveryID}); err != nil {
			t.Fatalf("receipt must allow retry: %v", err)
		}
	}
	if stats := b.Stats(); stats.Pending != 1 || stats.Queued != 0 {
		t.Fatalf("confirmed command remained queued: %+v", stats)
	}
	complete(t, b, testSession, second)
	if result := receive(t, call); result.err != nil {
		t.Fatal(result.err)
	}
	idle(t, b)
}

func TestReceiptCannotReviveRevokedEnrollment(t *testing.T) {
	b := broker(t, BrokerOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first := callAsync(b, ctx, testSession, Request{RequestID: "reused", Operation: "read"})
	command, err := b.poll(ctx, testSession, true)
	if err != nil {
		t.Fatal(err)
	}
	receipt := Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}
	if err = b.Revoke(testSession); err != nil {
		t.Fatal(err)
	}
	_ = receive(t, first)
	if err = b.Confirm(testSession, receipt); !errors.Is(err, ErrSession) {
		t.Fatal(err)
	}
	replacement := Session{NodeID: testSession.NodeID, EnrollmentID: "replacement"}
	if err = b.Activate(replacement); err != nil {
		t.Fatal(err)
	}
	next := callAsync(b, ctx, replacement, Request{RequestID: "reused", Operation: "read", Payload: json.RawMessage(`{}`)})
	newCommand := poll(t, b, replacement)
	if err = b.Confirm(replacement, receipt); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("old receipt revived new request: %v", err)
	}
	complete(t, b, replacement, newCommand)
	_ = receive(t, next)
	idle(t, b)
}
