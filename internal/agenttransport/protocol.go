// Package agenttransport provides an authenticated, bounded reverse command
// channel. It does not implement enrollment, business operations or storage.
package agenttransport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrClosed              = errors.New("agent broker is closed")
	ErrSession             = errors.New("agent enrollment is not active")
	ErrQueueFull           = errors.New("agent command queue is full")
	ErrPayloadTooLarge     = errors.New("agent payload exceeds the configured limit")
	ErrDuplicateRequest    = errors.New("request ID is already pending")
	ErrUnknownRequest      = errors.New("request is no longer pending")
	ErrNotDelivered        = errors.New("request has not been delivered")
	ErrInvalidRequest      = errors.New("invalid agent command")
	ErrIdempotencyConflict = errors.New("idempotency key was used with a different command")
	ErrJournalFull         = errors.New("idempotency journal is full")
)

// Session must come from the authenticated enrollment registry, never from a
// caller-supplied node address. EnrollmentID changes whenever a node rejoins.
type Session struct {
	NodeID       string `json:"nodeId"`
	EnrollmentID string `json:"enrollmentId"`
}

func (s Session) valid() bool {
	return s.NodeID != "" && s.EnrollmentID != "" && len(s.NodeID) <= 256 && len(s.EnrollmentID) <= 256 && !strings.ContainsAny(s.NodeID+s.EnrollmentID, "\x00\r\n")
}

type Request struct {
	RequestID      string          `json:"requestId"`
	Operation      string          `json:"operation"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Mutating       bool            `json:"mutating"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}
type Command struct {
	Session    Session `json:"session"`
	DeliveryID string  `json:"deliveryId"`
	Request
	DeadlineUnixMilli int64 `json:"deadlineUnixMilli"`
}
type Response struct {
	Status  int             `json:"status"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
type Result struct {
	DeliveryID string   `json:"deliveryId"`
	RequestID  string   `json:"requestId"`
	Response   Response `json:"response"`
}
type Receipt struct {
	DeliveryID string `json:"deliveryId"`
	RequestID  string `json:"requestId"`
}

// Executor invokes Go business services directly. Returning an error means the
// outcome may be unknown; definitive business failures belong in Response.
type Executor interface {
	Execute(context.Context, Command) (Response, error)
}

// ReplaySafeExecutor explicitly opts a local operation into its own durable
// reconciliation protocol. This is never a flag accepted from the peer. Such an
// operation must bind IDs/revisions to immutable payloads, commit before ACK,
// and reject stale generations even after restart. External side effects alone
// (for example issuing a PUT) are not replay safe.
type ReplaySafeExecutor interface {
	ReplaySafe(operation string) bool
}
type ExecutorFunc func(context.Context, Command) (Response, error)

func (f ExecutorFunc) Execute(ctx context.Context, c Command) (Response, error) { return f(ctx, c) }

type Transport interface {
	Poll(context.Context) (Command, error)
	Submit(context.Context, Result) error
}
