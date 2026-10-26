package agentservice

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const leaseBucket = "agent-parent-leases"
const MaxLeaseTTL = 60 * time.Second

var (
	ErrInvalidLease  = errors.New("invalid parent execution lease")
	ErrLeaseRequired = errors.New("parent execution lease is not active")
	ErrLeaseExpired  = errors.New("parent execution lease expired; a new lease ID is required")
	ErrLeaseRetired  = errors.New("this node is permanently retired from the parent task")
	ErrLeaseConflict = errors.New("stale or conflicting execution lease")
)

type LeasePrepareRequest struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	LeaseID    string `json:"leaseId"`
	Sequence   uint64 `json:"sequence"`
	TTLMillis  int64  `json:"ttlMs"`
}
type LeasePrepared struct {
	Challenge string `json:"challenge"`
	BootID    string `json:"bootId"`
	TTLMillis int64  `json:"ttlMs"`
}
type LeaseGrantRequest struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	LeaseID    string `json:"leaseId"`
	Sequence   uint64 `json:"sequence"`
	Challenge  string `json:"challenge"`
	BootID     string `json:"bootId"`
}
type LeaseRetireRequest struct {
	ParentID int64 `json:"parentId"`
}
type leaseRecord struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	LeaseID    string `json:"leaseId"`
	Sequence   uint64 `json:"sequence"`
	Required   bool   `json:"required"`
	Retired    bool   `json:"retired,omitempty"`
}
type leaseCandidate struct {
	request  LeasePrepareRequest
	result   LeasePrepared
	deadline time.Time
	granted  bool
}
type leaseTimer interface{ Stop() bool }
type parentLease struct {
	opMu            sync.Mutex
	record          leaseRecord
	ctx             context.Context
	cancel          context.CancelFunc
	active, expired bool
	deadline        time.Time
	pending         *leaseCandidate
	timer           leaseTimer
}

// Leases gates all parent I/O, including legacy work before its first lease.
// Expiry never waits for a database lock. Only required/retired ownership is
// durable: after restart no persisted lease authorizes execution.
type Leases struct {
	store     *state.Store
	bootID    string
	mu        sync.Mutex
	parents   map[int64]*parentLease
	closed    bool
	now       func() time.Time
	afterFunc func(time.Duration, func()) leaseTimer
	quiesce   func(context.Context, int64) error
}

func NewLeases(store *state.Store) (*Leases, error) {
	bootID, err := leaseNonce()
	if err != nil {
		return nil, err
	}
	m := &Leases{store: store, bootID: bootID, parents: map[int64]*parentLease{}, now: time.Now,
		afterFunc: func(d time.Duration, f func()) leaseTimer { return time.AfterFunc(d, f) }}
	after := ""
	for {
		count := 0
		err = store.View(func(tx *state.Tx) error {
			return tx.Scan(leaseBucket, "", after, 128, func(key string, raw json.RawMessage) error {
				var row leaseRecord
				if _, err := tx.Get(leaseBucket, key, &row); err != nil {
					return err
				}
				if row.ParentID < 1 || !row.Required {
					return errors.New("invalid durable parent lease")
				}
				m.parents[row.ParentID] = &parentLease{record: row, expired: row.LeaseID != ""}
				count++
				after = key
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		if count < 128 {
			return m, nil
		}
	}
}
func leaseNonce() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}
func leaseKey(parent int64) string { return fmt.Sprintf("%020d", parent) }
func (m *Leases) entryLocked(parent int64) *parentLease {
	p := m.parents[parent]
	if p == nil {
		ctx, cancel := context.WithCancel(context.Background())
		p = &parentLease{record: leaseRecord{ParentID: parent}, ctx: ctx, cancel: cancel}
		m.parents[parent] = p
	}
	return p
}
func (m *Leases) closeLocked(p *parentLease) {
	p.active = false
	if p.cancel != nil {
		p.cancel()
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}
func (m *Leases) expireLocked(p *parentLease) {
	if p.active && !m.now().Before(p.deadline) {
		p.expired = true
		m.closeLocked(p)
	}
}
func (m *Leases) ParentContext(parentID int64, generation uint64) (context.Context, error) {
	if m == nil {
		return context.Background(), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if parentID < 1 || generation == 0 {
		return nil, ErrInvalidLease
	}
	if m.closed {
		return nil, ErrLeaseRequired
	}
	p := m.entryLocked(parentID)
	m.expireLocked(p)
	if p.record.Retired {
		return nil, ErrLeaseRetired
	}
	if !p.record.Required {
		return p.ctx, nil
	}
	if p.expired {
		return nil, ErrLeaseExpired
	}
	if !p.active || p.record.Generation != generation {
		return nil, ErrLeaseRequired
	}
	return p.ctx, nil
}
func (m *Leases) Admission(parentID int64, generation uint64) error {
	_, err := m.ParentContext(parentID, generation)
	return err
}
func (m *Leases) persist(row leaseRecord) error {
	return m.store.Update(func(tx *state.Tx) error {
		if err := state.RequireSchema(tx, "agent", state.SchemaTaskLeases); err != nil {
			return err
		}
		return tx.Put(leaseBucket, leaseKey(row.ParentID), row)
	})
}
func (m *Leases) Prepare(ctx context.Context, request LeasePrepareRequest) (LeasePrepared, error) {
	if request.ParentID < 1 || request.Generation == 0 || request.LeaseID == "" || len(request.LeaseID) > 128 || request.Sequence == 0 || request.TTLMillis < 1 || request.TTLMillis > MaxLeaseTTL.Milliseconds() {
		return LeasePrepared{}, ErrInvalidLease
	}
	m.mu.Lock()
	p := m.entryLocked(request.ParentID)
	m.mu.Unlock()
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return LeasePrepared{}, err
	}
	m.mu.Lock()
	m.expireLocked(p)
	if m.closed || p.record.Retired {
		m.mu.Unlock()
		return LeasePrepared{}, ErrLeaseRetired
	}
	if request.Generation < p.record.Generation {
		m.mu.Unlock()
		return LeasePrepared{}, ErrLeaseConflict
	}
	same := p.record.LeaseID == request.LeaseID && p.record.Generation == request.Generation
	if p.record.LeaseID == request.LeaseID && p.expired {
		m.mu.Unlock()
		return LeasePrepared{}, ErrLeaseExpired
	}
	if p.pending != nil && p.pending.request == request {
		result, alive := p.pending.result, m.now().Before(p.pending.deadline)
		m.mu.Unlock()
		if !alive {
			return LeasePrepared{}, ErrLeaseExpired
		}
		return result, nil
	}
	if same && (request.Sequence <= p.record.Sequence || p.pending != nil && request.Sequence <= p.pending.request.Sequence) {
		m.mu.Unlock()
		return LeasePrepared{}, ErrLeaseConflict
	}
	rearm := !same || !p.active
	if rearm {
		m.closeLocked(p)
		p.record = leaseRecord{ParentID: request.ParentID, Generation: request.Generation, LeaseID: request.LeaseID, Required: true}
		p.expired, p.pending = false, nil
	}
	row := p.record
	m.mu.Unlock()
	if rearm {
		// This marker also raises the DB schema before acknowledging lease support,
		// preventing an older Agent binary from reopening and running the old queue.
		if err := m.persist(row); err != nil {
			return LeasePrepared{}, err
		}
		if m.quiesce != nil {
			if err := m.quiesce(ctx, request.ParentID); err != nil {
				return LeasePrepared{}, err
			}
		}
	}
	challenge, err := leaseNonce()
	if err != nil {
		return LeasePrepared{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.record.Retired || m.closed {
		return LeasePrepared{}, ErrLeaseRetired
	}
	m.expireLocked(p)
	if p.expired {
		return LeasePrepared{}, ErrLeaseExpired
	}
	if err := ctx.Err(); err != nil {
		return LeasePrepared{}, err
	}
	result := LeasePrepared{Challenge: challenge, BootID: m.bootID, TTLMillis: request.TTLMillis}
	p.pending = &leaseCandidate{request: request, result: result, deadline: m.now().Add(time.Duration(request.TTLMillis) * time.Millisecond)}
	return result, nil
}
func (m *Leases) Grant(ctx context.Context, request LeaseGrantRequest) error {
	if request.ParentID < 1 || request.Generation == 0 || request.LeaseID == "" || request.Sequence == 0 || request.Challenge == "" || request.BootID == "" {
		return ErrInvalidLease
	}
	m.mu.Lock()
	p := m.parents[request.ParentID]
	m.mu.Unlock()
	if p == nil {
		return ErrLeaseRequired
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.expireLocked(p)
	if m.closed || p.record.Retired {
		m.mu.Unlock()
		return ErrLeaseRetired
	}
	if p.expired {
		m.mu.Unlock()
		return ErrLeaseExpired
	}
	candidate := p.pending
	if candidate == nil || request.BootID != m.bootID || request.Challenge != candidate.result.Challenge || request.LeaseID != candidate.request.LeaseID || request.Generation != candidate.request.Generation || request.Sequence != candidate.request.Sequence {
		m.mu.Unlock()
		return ErrLeaseConflict
	}
	if !m.now().Before(candidate.deadline) {
		m.mu.Unlock()
		return ErrLeaseExpired
	}
	if candidate.granted {
		m.mu.Unlock()
		return nil
	}
	row := p.record
	row.Sequence = request.Sequence
	m.mu.Unlock()
	if err := m.persist(row); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(p)
	if p.record.Retired || m.closed {
		return ErrLeaseRetired
	}
	if p.expired || !m.now().Before(candidate.deadline) {
		return ErrLeaseExpired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.ctx == nil || p.ctx.Err() != nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	p.record, p.deadline, p.active = row, candidate.deadline, true
	candidate.granted = true
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = m.afterFunc(p.deadline.Sub(m.now()), func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if p.record.LeaseID == request.LeaseID && p.record.Sequence == request.Sequence {
			m.expireLocked(p)
		}
	})
	return nil
}
func (m *Leases) Retire(ctx context.Context, parentID int64) error {
	if parentID < 1 {
		return ErrInvalidLease
	}
	// Revoke in memory before contending with grants or the DB writer.
	m.mu.Lock()
	p := m.entryLocked(parentID)
	p.record.Required, p.record.Retired = true, true
	m.closeLocked(p)
	m.mu.Unlock()
	p.opMu.Lock()
	defer p.opMu.Unlock()
	m.mu.Lock()
	row := p.record
	m.mu.Unlock()
	if err := m.persist(row); err != nil {
		return err
	}
	if m.quiesce != nil {
		return m.quiesce(ctx, parentID)
	}
	return nil
}
func (m *Leases) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, p := range m.parents {
		m.closeLocked(p)
	}
}
func (s *Service) LeaseCapabilities() map[string]any {
	if s.Leases == nil {
		return nil
	}
	return map[string]any{"leaseProtocol": 1, "leaseBootId": s.Leases.bootID}
}
