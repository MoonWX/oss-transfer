package agenttransport

import (
	"context"
	"encoding/json"
	"sync"
)

type JournalState string

const (
	JournalRunning   JournalState = "running"
	JournalCompleted JournalState = "completed"
	JournalUnknown   JournalState = "outcome_unknown"
)

type Claim struct {
	Acquired bool
	State    JournalState
	Response Response
}

// Journal must atomically claim an idempotency key and durably record the result
// before an acknowledgment. On restart, interrupted running claims become unknown,
// never available for execution again. A durable implementation is needed for
// restart protection. This contract does not make external side effects atomic.
type Journal interface {
	Begin(context.Context, string, string) (Claim, error)
	Complete(context.Context, string, string, Response) error
	Unknown(context.Context, string, string) error
}
type journalEntry struct {
	fingerprint string
	state       JournalState
	response    Response
}

// MemoryJournal is bounded process-local deduplication for tests and ephemeral
// agents. It cannot prevent duplicate mutations after a process restart. Completed
// keys are never silently evicted: a full journal refuses new mutations.
type MemoryJournal struct {
	mu         sync.Mutex
	entries    map[string]journalEntry
	maxEntries int
	bytes      int64
	maxBytes   int64
}

func NewMemoryJournal(maxEntries int) *MemoryJournal {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &MemoryJournal{entries: map[string]journalEntry{}, maxEntries: maxEntries, maxBytes: 32 << 20}
}
func copyResponse(r Response) Response {
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r
}
func (j *MemoryJournal) Begin(ctx context.Context, key, fp string) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e, ok := j.entries[key]; ok {
		if e.fingerprint != fp {
			return Claim{}, ErrIdempotencyConflict
		}
		return Claim{State: e.state, Response: copyResponse(e.response)}, nil
	}
	if len(j.entries) >= j.maxEntries {
		return Claim{}, ErrJournalFull
	}
	j.entries[key] = journalEntry{fingerprint: fp, state: JournalRunning}
	return Claim{Acquired: true, State: JournalRunning}, nil
}
func (j *MemoryJournal) Complete(ctx context.Context, key, fp string, r Response) error {
	return j.change(ctx, key, fp, JournalCompleted, r)
}
func (j *MemoryJournal) Unknown(ctx context.Context, key, fp string) error {
	return j.change(ctx, key, fp, JournalUnknown, Response{})
}
func (j *MemoryJournal) change(ctx context.Context, key, fp string, state JournalState, r Response) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[key]
	if !ok || e.fingerprint != fp {
		return ErrIdempotencyConflict
	}
	if e.state == JournalCompleted {
		return nil
	}
	if int64(len(r.Payload)) > j.maxBytes-j.bytes+int64(len(e.response.Payload)) {
		return ErrJournalFull
	}
	j.bytes += int64(len(r.Payload)) - int64(len(e.response.Payload))
	e.state = state
	e.response = copyResponse(r)
	j.entries[key] = e
	return nil
}
