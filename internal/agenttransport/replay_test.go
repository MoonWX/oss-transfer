package agenttransport

import (
	"context"
	"testing"
	"time"
)

type durableExecutor struct{ called int }

func (e *durableExecutor) ReplaySafe(op string) bool { return op == "transactional_accept" }
func (e *durableExecutor) Execute(context.Context, Command) (Response, error) {
	e.called++
	return Response{Status: 200}, nil
}
func TestOnlyLocalExecutorCanSelectDurableReconciliation(t *testing.T) {
	e := &durableExecutor{}
	r := NewRunner(nil, e, nil, RunnerOptions{})
	c := Command{Session: testSession, DeliveryID: "d", Request: Request{RequestID: "r", Operation: "transactional_accept", Mutating: true, IdempotencyKey: "id"}, DeadlineUnixMilli: time.Now().Add(time.Minute).UnixMilli()}
	if got := r.runCommand(context.Background(), c); got.Status != 200 {
		t.Fatal(got)
	}
	c.IdempotencyKey = ""
	if got := r.runCommand(context.Background(), c); got.Status != 400 {
		t.Fatal(got)
	}
	c.IdempotencyKey = "id"
	c.Operation = "external_side_effect"
	if got := r.runCommand(context.Background(), c); got.Status != 503 {
		t.Fatal(got)
	}
	if e.called != 1 {
		t.Fatal("unsafe operation reached executor")
	}
}
