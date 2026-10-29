package master

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

const taskLeaseTTL = 60 * time.Second
const taskLeaseMargin = 10 * time.Second

type taskLeaseRecord struct {
	ParentID   int64     `json:"parentId"`
	Node       task.Node `json:"node"`
	Generation uint64    `json:"generation"`
	LeaseID    string    `json:"leaseId"`
	Sequence   uint64    `json:"sequence"`
	Deadline   time.Time `json:"deadline"`
	Armed      bool      `json:"armed"`
	Retired    bool      `json:"retired,omitempty"`
}
type taskLeaseEntry struct {
	mu            sync.Mutex
	record        taskLeaseRecord
	safeAfter     time.Time // Monotonic. Persisted wall time is never used to shorten this fence.
	renewAt       time.Time
	retireAck     bool
	wake          chan struct{}
	workerStarted bool // guarded by taskLeases.mu
}
type taskLeases struct {
	runtime       *taskRuntime
	mu            sync.Mutex
	entries       map[string]*taskLeaseEntry
	ttl, margin   time.Duration
	now           func() time.Time
	wake          chan struct{}
	retryInterval time.Duration
	call          func(context.Context, agenttransport.Session, string, bool, any, any) error
}

func newTaskLeases(t *taskRuntime) *taskLeases {
	return &taskLeases{runtime: t, entries: map[string]*taskLeaseEntry{}, ttl: taskLeaseTTL, margin: taskLeaseMargin, now: time.Now, wake: make(chan struct{}, 1), retryInterval: time.Second, call: t.server.agentCall}
}
func taskLeaseKey(parentID int64, nodeID string) string {
	return fmt.Sprintf("%020d/%s", parentID, nodeID)
}
func newLeaseID() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (l *taskLeases) entry(p task.Parent, node task.Node) (*taskLeaseEntry, error) {
	key := taskLeaseKey(p.ID, node.ID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[key]; e != nil {
		return e, nil
	}
	e := &taskLeaseEntry{wake: make(chan struct{}, 1)}
	var found bool
	err := l.runtime.server.store.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get("master-task-leases", key, &e.record)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		e.record = taskLeaseRecord{ParentID: p.ID, Node: node, Generation: p.Generation}
	} else {
		// A Master restart loses monotonic deadlines. Wait a full lease even when
		// the wall clock advanced or the stored grant response was lost.
		e.safeAfter = l.now().Add(l.ttl + l.margin)
		e.record.LeaseID, err = newLeaseID()
		if err != nil {
			return nil, err
		}
		e.record.Sequence = 0
	}
	l.entries[key] = e
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return e, nil
}
func (l *taskLeases) save(e *taskLeaseEntry) error {
	return l.runtime.server.store.Update(func(tx *state.Tx) error {
		if err := allowTaskMetadataWrite(tx, e.record.ParentID); err != nil {
			return err
		}
		return tx.Put("master-task-leases", taskLeaseKey(e.record.ParentID, e.record.Node.ID), e.record)
	})
}
func leasePeerExpired(err error) bool {
	if errors.Is(err, agentservice.ErrLeaseExpired) {
		return true
	}
	var remote *AgentError
	return errors.As(err, &remote) && remote.Status == 409 && remote.Message == agentservice.ErrLeaseExpired.Error()
}
func wakeLease(e *taskLeaseEntry) {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (l *taskLeases) renew(ctx context.Context, p task.Parent, node task.Node) error {
	e, err := l.entry(p, node)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// A queued renewer may hold a RUNNING snapshot from before Stop/resume.
	// Re-read while sharing the same lock as expiration/retirement decisions.
	current, err := l.runtime.repo.Get(p.ID)
	if err != nil {
		return err
	}
	if current.Generation != p.Generation {
		return task.ErrGeneration
	}
	if current.Deleted || task.Ended(current.Status) || current.Status == "STOPPING" {
		return task.ErrState
	}
	p = current
	if e.record.Retired || p.NodeRetired(node.ID) {
		return errors.New("task ownership on this node has been retired")
	}
	now := l.now()
	if now.Before(e.renewAt) && e.record.Generation == p.Generation {
		wakeLease(e)
		return nil
	}
	fresh := e.record.LeaseID == "" || !now.Before(e.safeAfter.Add(-l.margin)) || e.record.Generation != p.Generation
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// An Agent restart forgets monotonic authority and rejects its old lease ID.
	// Rearm immediately after that explicit rejection; never wait out a healthy
	// node's old lease, and never discard its conservative retirement fence.
	for attempt := 0; attempt < 2; attempt++ {
		if fresh {
			e.record.LeaseID, err = newLeaseID()
			if err != nil {
				return err
			}
			e.record.Sequence = 0
		}
		e.record.Generation = p.Generation
		e.record.Sequence++
		request := agentservice.LeasePrepareRequest{ParentID: p.ID, Generation: p.Generation, LeaseID: e.record.LeaseID, Sequence: e.record.Sequence, TTLMillis: l.ttl.Milliseconds()}
		var ready agentservice.LeasePrepared
		err = l.call(callCtx, nodeSession(node), "lease_prepare", true, request, &ready)
		if leasePeerExpired(err) && attempt == 0 {
			fresh = true
			continue
		}
		if err != nil {
			return err
		}
		if ready.Challenge == "" || ready.BootID == "" || ready.TTLMillis != request.TTLMillis {
			return errors.New("invalid Agent task lease response")
		}
		// The Agent fixed its deadline before its response. A restarted process or
		// a delayed response cannot shorten any previous unconfirmed grant's fence.
		now = l.now()
		if next := now.Add(l.ttl + l.margin); next.After(e.safeAfter) {
			e.safeAfter = next
		}
		e.record.Deadline = e.safeAfter.UTC()
		e.record.Armed = true
		if err = l.save(e); err != nil {
			return err
		}
		grant := agentservice.LeaseGrantRequest{ParentID: p.ID, Generation: p.Generation, LeaseID: e.record.LeaseID, Sequence: e.record.Sequence, Challenge: ready.Challenge, BootID: ready.BootID}
		err = l.call(callCtx, nodeSession(node), "lease_grant", true, grant, nil)
		if leasePeerExpired(err) && attempt == 0 {
			fresh = true
			continue
		}
		if err != nil {
			return err
		}
		e.renewAt = now.Add(l.ttl / 3)
		wakeLease(e)
		return nil
	}
	return err
}
func (l *taskLeases) expired(p task.Parent, node task.Node) (bool, error) {
	e, err := l.entry(p, node)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Never reassign work that might predate the lease protocol.
	return e.record.Armed && !e.safeAfter.IsZero() && !l.now().Before(e.safeAfter), nil
}
func (l *taskLeases) retire(p task.Parent, node task.Node) error {
	e, err := l.entry(p, node)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.record.Retired {
		return nil
	}
	if !e.record.Armed || e.safeAfter.IsZero() || l.now().Before(e.safeAfter) {
		return errors.New("waiting for the previous node's task lease to expire")
	}
	e.record.Retired = true
	defer wakeLease(e)
	if err = l.save(e); err != nil {
		e.record.Retired = false
		return err
	}
	return nil
}

// Each lease owns exactly one worker. A slow peer cannot occupy a shared
// worker slot or delay the next renewal of another peer. Finished tasks park
// until an explicit renewal wakes them; process shutdown cancels every worker.
func (l *taskLeases) loop(ctx context.Context) {
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		l.mu.Lock()
		for _, entry := range l.entries {
			if entry.workerStarted {
				continue
			}
			entry.workerStarted = true
			workers.Add(1)
			go func(e *taskLeaseEntry) { defer workers.Done(); l.runLease(ctx, e) }(entry)
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		}
	}
}
func (l *taskLeases) runLease(ctx context.Context, e *taskLeaseEntry) {
	for ctx.Err() == nil {
		delay, park := l.leaseTick(ctx, e)
		var timer *time.Timer
		var tick <-chan time.Time
		if !park {
			timer = time.NewTimer(max(delay, time.Millisecond))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-e.wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
func (l *taskLeases) leaseTick(ctx context.Context, e *taskLeaseEntry) (time.Duration, bool) {
	e.mu.Lock()
	row, ack, next := e.record, e.retireAck, e.renewAt
	e.mu.Unlock()
	if row.Retired {
		if ack {
			return 0, true
		}
		if wait := next.Sub(l.now()); wait > 0 {
			return wait, false
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := l.call(callCtx, nodeSession(row.Node), "lease_retire", true, map[string]any{"parentId": row.ParentID}, nil)
		cancel()
		e.mu.Lock()
		e.retireAck = err == nil
		e.renewAt = l.now().Add(15 * time.Second)
		e.mu.Unlock()
		return 15 * time.Second, err == nil
	}
	p, err := l.runtime.repo.Get(row.ParentID)
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			return 0, true
		}
		return l.retryInterval, false
	}
	if p.Deleted || task.Ended(p.Status) || p.Failover == nil || !p.Failover.Enabled || p.NodeRetired(row.Node.ID) {
		return 0, true
	}
	if p.Status == "STOPPING" {
		return l.retryInterval, false
	}
	if wait := next.Sub(l.now()); wait > 0 {
		return min(wait, 5*time.Second), false
	}
	if err := l.renew(ctx, p, row.Node); err != nil {
		return l.retryInterval, false
	}
	e.mu.Lock()
	next = e.renewAt
	e.mu.Unlock()
	return max(next.Sub(l.now()), time.Millisecond), false
}
