package agenttransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type RunnerOptions struct {
	MaxPayloadBytes int
	Concurrency     int
	ReconnectDelay  time.Duration
	ResultAttempts  int
	ResultTimeout   time.Duration
}
type Runner struct {
	transport Transport
	executor  Executor
	journal   Journal
	options   RunnerOptions
}

// A Journal is mandatory for mutation handling. Supplying nil permits reads but
// makes every mutating command fail closed, before invoking the executor.
func NewRunner(t Transport, e Executor, j Journal, o RunnerOptions) *Runner {
	if o.MaxPayloadBytes <= 0 {
		o.MaxPayloadBytes = 8 << 20
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = time.Second
	}
	if o.ResultAttempts <= 0 {
		o.ResultAttempts = 3
	}
	if o.ResultTimeout <= 0 {
		o.ResultTimeout = 10 * time.Second
	}
	return &Runner{transport: t, executor: e, journal: j, options: o}
}
func (r *Runner) Run(ctx context.Context) error {
	if r.transport == nil || r.executor == nil {
		return ErrInvalidRequest
	}
	ctx, cancel := context.WithCancel(ctx)
	slots := make(chan struct{}, r.options.Concurrency)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case slots <- struct{}{}:
		}
		c, err := r.transport.Poll(ctx)
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrSession) {
				return ErrSession
			}
			if errors.Is(err, ErrNoCommand) {
				continue
			}
			if !wait(ctx, r.options.ReconnectDelay) {
				return ctx.Err()
			}
			continue
		}
		workers.Add(1)
		go func() { defer workers.Done(); defer func() { <-slots }(); r.execute(ctx, c) }()
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func problem(status int, code string) Response {
	body, _ := json.Marshal(map[string]string{"code": code})
	return Response{Status: status, Payload: body}
}
func fingerprint(c Command) string {
	sum := sha256.Sum256(append([]byte(c.Operation+"\x00"), c.Payload...))
	return hex.EncodeToString(sum[:])
}
func (r *Runner) execute(parent context.Context, c Command) {
	response := r.runCommand(parent, c)
	// Only result delivery is retried. Business execution is never retried here.
	for attempt := 0; attempt < r.options.ResultAttempts && parent.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(parent, r.options.ResultTimeout)
		err := r.transport.Submit(ctx, Result{DeliveryID: c.DeliveryID, RequestID: c.RequestID, Response: response})
		cancel()
		if err == nil {
			return
		}
		if !wait(parent, r.options.ReconnectDelay) {
			return
		}
	}
}
func (r *Runner) runCommand(parent context.Context, c Command) Response {
	if len(c.Payload) > r.options.MaxPayloadBytes {
		return problem(413, "payload_too_large")
	}
	if !c.Session.valid() || c.DeliveryID == "" || c.RequestID == "" || len(c.RequestID) > 256 || c.Operation == "" || len(c.Operation) > 256 || c.DeadlineUnixMilli <= 0 || (len(c.Payload) > 0 && !json.Valid(c.Payload)) {
		return problem(400, "invalid_command")
	}
	ctx, cancel := context.WithDeadline(parent, time.UnixMilli(c.DeadlineUnixMilli))
	defer cancel()
	if ctx.Err() != nil {
		return problem(408, "command_expired")
	}
	fp := fingerprint(c)
	journalKey := c.Session.NodeID + "\x00" + c.Session.EnrollmentID + "\x00" + c.IdempotencyKey
	useJournal := c.Mutating
	if durable, ok := r.executor.(ReplaySafeExecutor); ok && durable.ReplaySafe(c.Operation) {
		useJournal = false
	}
	if c.Mutating {
		if c.IdempotencyKey == "" || len(c.IdempotencyKey) > 256 {
			return problem(400, "idempotency_key_required")
		}
	}
	if useJournal {
		if r.journal == nil {
			return problem(503, "journal_required")
		}
		claim, err := r.journal.Begin(ctx, journalKey, fp)
		if errors.Is(err, ErrIdempotencyConflict) {
			return problem(409, "idempotency_conflict")
		}
		if err != nil {
			return problem(503, "journal_unavailable")
		}
		if !claim.Acquired {
			if claim.State == JournalCompleted {
				return claim.Response
			}
			return problem(409, "outcome_unknown")
		}
	}
	response, err := invoke(ctx, r.executor, c)
	if err == nil && len(response.Payload) > r.options.MaxPayloadBytes {
		err = ErrPayloadTooLarge
	}
	if useJournal {
		// A cancelled command may already have modified storage. Retain the claim even
		// during shutdown; a future delivery must not repeat the side effect blindly.
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer saveCancel()
		if err != nil {
			_ = r.journal.Unknown(saveCtx, journalKey, fp)
			return problem(409, "outcome_unknown")
		}
		if err = r.journal.Complete(saveCtx, journalKey, fp, response); err != nil {
			_ = r.journal.Unknown(saveCtx, journalKey, fp)
			return problem(409, "outcome_unknown")
		}
	}
	if err != nil {
		return problem(502, "command_failed")
	}
	return response
}
func invoke(ctx context.Context, e Executor, c Command) (response Response, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("executor panicked")
		}
	}()
	response, err = e.Execute(ctx, c)
	if err == nil && (response.Status < 100 || response.Status > 599 || (len(response.Payload) > 0 && !json.Valid(response.Payload))) {
		err = ErrInvalidRequest
	}
	return
}
