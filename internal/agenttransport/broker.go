package agenttransport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type BrokerOptions struct {
	MaxPending         int
	MaxQueuedPerNode   int
	MaxPayloadBytes    int
	MaxPendingBytes    int64
	MaxCommandDuration time.Duration
	// DeliveryLease bounds how long an HTTP poll may hold a command without
	// confirming receipt. Confirmed executions are never redelivered by the broker.
	DeliveryLease time.Duration
}

func (o BrokerOptions) defaults() BrokerOptions {
	if o.MaxPending <= 0 {
		o.MaxPending = 1024
	}
	if o.MaxQueuedPerNode <= 0 {
		o.MaxQueuedPerNode = 64
	}
	if o.MaxPayloadBytes <= 0 {
		o.MaxPayloadBytes = 8 << 20
	}
	if o.MaxPendingBytes <= 0 {
		o.MaxPendingBytes = 32 << 20
	}
	if o.MaxCommandDuration <= 0 {
		o.MaxCommandDuration = 2 * time.Minute
	}
	if o.DeliveryLease <= 0 {
		o.DeliveryLease = 5 * time.Second
	}
	return o
}

type delivery struct {
	response Response
	err      error
}
type pending struct {
	ctx       context.Context
	session   Session
	command   Command
	delivered bool
	confirmed bool
	leaseEnd  time.Time
	cost      int64
	result    chan delivery
}
type lane struct {
	session Session
	queue   []*pending
}
type Broker struct {
	mu      sync.Mutex
	options BrokerOptions
	lanes   map[string]*lane
	pending map[string]*pending
	bytes   int64
	changed chan struct{}
	closed  bool
}
type BrokerStats struct {
	Sessions, Pending, Queued int
	PendingBytes              int64
}

func NewBroker(options BrokerOptions) *Broker {
	return &Broker{options: options.defaults(), lanes: map[string]*lane{}, pending: map[string]*pending{}, changed: make(chan struct{})}
}
func (b *Broker) signal() { close(b.changed); b.changed = make(chan struct{}) }

// Activate is called only after the registry has committed the new enrollment.
// It invalidates all requests from the previous enrollment at this node.
func (b *Broker) Activate(s Session) error {
	if !s.valid() {
		return ErrSession
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if old := b.lanes[s.NodeID]; old != nil {
		if old.session == s {
			return nil
		}
		b.invalidate(old.session, ErrSession)
	}
	b.lanes[s.NodeID] = &lane{session: s}
	b.signal()
	return nil
}
func (b *Broker) Revoke(s Session) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active(s) {
		return ErrSession
	}
	b.invalidate(s, ErrSession)
	delete(b.lanes, s.NodeID)
	b.signal()
	return nil
}
func (b *Broker) active(s Session) bool { l := b.lanes[s.NodeID]; return l != nil && l.session == s }
func (b *Broker) invalidate(s Session, err error) {
	for _, p := range b.pending {
		if p.session == s {
			b.finish(p, delivery{err: err})
		}
	}
}

// finish owns all map/queue accounting. The buffered send never waits for a caller.
func (b *Broker) finish(p *pending, d delivery) {
	if b.pending[p.command.RequestID] != p {
		return
	}
	delete(b.pending, p.command.RequestID)
	b.bytes -= p.cost
	if l := b.lanes[p.session.NodeID]; l != nil && l.session == p.session {
		for i, v := range l.queue {
			if v == p {
				copy(l.queue[i:], l.queue[i+1:])
				l.queue[len(l.queue)-1] = nil
				l.queue = l.queue[:len(l.queue)-1]
				break
			}
		}
	}
	p.result <- d
	b.signal()
}
func (b *Broker) Call(ctx context.Context, s Session, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if r.RequestID == "" {
		var v [16]byte
		if _, err := rand.Read(v[:]); err != nil {
			return Response{}, err
		}
		r.RequestID = hex.EncodeToString(v[:])
	}
	if len(r.RequestID) > 256 || r.Operation == "" || len(r.Operation) > 256 || len(r.IdempotencyKey) > 256 || (r.Mutating && r.IdempotencyKey == "") || (len(r.Payload) > 0 && !json.Valid(r.Payload)) {
		return Response{}, ErrInvalidRequest
	}
	if len(r.Payload) > b.options.MaxPayloadBytes {
		return Response{}, ErrPayloadTooLarge
	}
	end := time.Now().Add(b.options.MaxCommandDuration)
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	callCtx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	var deliveryToken [16]byte
	if _, err := rand.Read(deliveryToken[:]); err != nil {
		return Response{}, err
	}
	p := &pending{ctx: callCtx, session: s, command: Command{Session: s, DeliveryID: hex.EncodeToString(deliveryToken[:]), Request: r, DeadlineUnixMilli: end.UnixMilli()}, cost: int64(len(r.Payload)) + 1024, result: make(chan delivery, 1)}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Response{}, ErrClosed
	}
	if !b.active(s) {
		b.mu.Unlock()
		return Response{}, ErrSession
	}
	l := b.lanes[s.NodeID]
	if b.pending[r.RequestID] != nil {
		b.mu.Unlock()
		return Response{}, ErrDuplicateRequest
	}
	if len(b.pending) >= b.options.MaxPending || len(l.queue) >= b.options.MaxQueuedPerNode || p.cost > b.options.MaxPendingBytes-b.bytes {
		b.mu.Unlock()
		return Response{}, ErrQueueFull
	}
	b.pending[r.RequestID] = p
	b.bytes += p.cost
	l.queue = append(l.queue, p)
	b.signal()
	b.mu.Unlock()
	select {
	case d := <-p.result:
		return d.response, d.err
	case <-callCtx.Done():
		b.mu.Lock()
		b.finish(p, delivery{err: callCtx.Err()})
		b.mu.Unlock()
		d := <-p.result
		return d.response, d.err
	}
}
func (b *Broker) Poll(ctx context.Context, s Session) (Command, error) {
	return b.poll(ctx, s, false)
}

// poll's receipt mode leaves commands in the bounded queue until the HTTP
// client confirms their delivery token. A successful server write does not
// establish that a disconnected client actually received the command.
func (b *Broker) poll(ctx context.Context, s Session, receipt bool) (Command, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Command{}, err
		}
		b.mu.Lock()
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return Command{}, err
		}
		if b.closed {
			b.mu.Unlock()
			return Command{}, ErrClosed
		}
		if !b.active(s) {
			b.mu.Unlock()
			return Command{}, ErrSession
		}
		l := b.lanes[s.NodeID]
		var nextLease time.Time
		for i := 0; i < len(l.queue); {
			p := l.queue[i]
			if err := p.ctx.Err(); err != nil {
				b.finish(p, delivery{err: err})
				continue
			}
			if time.Now().UnixMilli() >= p.command.DeadlineUnixMilli {
				b.finish(p, delivery{err: context.DeadlineExceeded})
				continue
			}
			if p.leaseEnd.After(time.Now()) {
				if nextLease.IsZero() || p.leaseEnd.Before(nextLease) {
					nextLease = p.leaseEnd
				}
				i++
				continue
			}
			if p.delivered {
				var token [16]byte
				if _, err := rand.Read(token[:]); err != nil {
					b.mu.Unlock()
					return Command{}, err
				}
				p.command.DeliveryID = hex.EncodeToString(token[:])
			}
			p.delivered = true
			p.confirmed = !receipt
			if receipt {
				p.leaseEnd = time.Now().Add(b.options.DeliveryLease)
			} else {
				copy(l.queue[i:], l.queue[i+1:])
				l.queue[len(l.queue)-1] = nil
				l.queue = l.queue[:len(l.queue)-1]
			}
			c := p.command
			c.Payload = append(json.RawMessage(nil), c.Payload...)
			b.mu.Unlock()
			return c, nil
		}
		changed := b.changed
		b.mu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !nextLease.IsZero() {
			timer = time.NewTimer(time.Until(nextLease))
			expired = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return Command{}, ctx.Err()
		case <-changed:
		case <-expired:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Confirm accepts retries of the same receipt but rejects a token replaced by
// redelivery. The client must not execute a command whose confirmation failed.
func (b *Broker) Confirm(s Session, r Receipt) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if !b.active(s) {
		return ErrSession
	}
	p := b.pending[r.RequestID]
	if p == nil || p.session != s || p.command.DeliveryID != r.DeliveryID {
		return ErrUnknownRequest
	}
	if !p.delivered {
		return ErrNotDelivered
	}
	if p.ctx.Err() != nil || time.Now().UnixMilli() >= p.command.DeadlineUnixMilli {
		return ErrUnknownRequest
	}
	if !p.confirmed && !p.leaseEnd.After(time.Now()) {
		return ErrUnknownRequest
	}
	if !p.confirmed {
		p.confirmed = true
		l := b.lanes[s.NodeID]
		for i, queued := range l.queue {
			if queued == p {
				copy(l.queue[i:], l.queue[i+1:])
				l.queue[len(l.queue)-1] = nil
				l.queue = l.queue[:len(l.queue)-1]
				break
			}
		}
		b.signal()
	}
	return nil
}
func (b *Broker) Complete(s Session, r Result) error {
	if len(r.Response.Payload) > b.options.MaxPayloadBytes {
		return ErrPayloadTooLarge
	}
	if r.Response.Status < 100 || r.Response.Status > 599 || (len(r.Response.Payload) > 0 && !json.Valid(r.Response.Payload)) {
		return ErrInvalidRequest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if !b.active(s) {
		return ErrSession
	}
	p := b.pending[r.RequestID]
	if p == nil || p.command.DeliveryID != r.DeliveryID {
		return ErrUnknownRequest
	}
	if p.session != s {
		return ErrSession
	}
	if !p.delivered || !p.confirmed {
		return ErrNotDelivered
	}
	if time.Now().UnixMilli() >= p.command.DeadlineUnixMilli {
		b.finish(p, delivery{err: context.DeadlineExceeded})
		return ErrUnknownRequest
	}
	r.Response.Payload = append(json.RawMessage(nil), r.Response.Payload...)
	b.finish(p, delivery{response: r.Response})
	return nil
}
func (b *Broker) Stats() BrokerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BrokerStats{Sessions: len(b.lanes), Pending: len(b.pending), PendingBytes: b.bytes}
	for _, l := range b.lanes {
		s.Queued += len(l.queue)
	}
	return s
}
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, p := range b.pending {
		b.finish(p, delivery{err: ErrClosed})
	}
	b.lanes = map[string]*lane{}
	b.signal()
	return nil
}
func (b *Broker) String() string {
	s := b.Stats()
	return fmt.Sprintf("agent broker: %d pending, %d queued", s.Pending, s.Queued)
}
