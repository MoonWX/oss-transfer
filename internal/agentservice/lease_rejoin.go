package agentservice

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
)

type LeaseRejoinRequest struct {
	ScannerEpoch   *uint64 `json:"scannerEpoch,omitempty"`
	ScannerThrough int64   `json:"scannerThrough,omitempty"`
	ParentID       int64   `json:"parentId"`
	Generation     uint64  `json:"generation"`
	OwnerEpoch     uint64  `json:"ownerEpoch"`
}

func (m *Leases) JobContext(job execution.Job) (context.Context, error) {
	return m.ownerContext(job.ParentID, job.Generation, &job.OwnerEpoch)
}
func (m *Leases) SpecContext(spec discovery.Spec) (context.Context, error) {
	return m.ownerContext(spec.ParentID, spec.Generation, &spec.OwnerEpoch)
}
func (m *Leases) AdmissionJob(job execution.Job) error {
	_, err := m.JobContext(job)
	return err
}

// Rejoin revokes all old I/O before acknowledging a newer ownership epoch.
// The durable pending marker survives a crash in quiescence. No execution lease
// is activated here; Master must separately grant the acknowledged epoch.
func (m *Leases) Rejoin(ctx context.Context, req LeaseRejoinRequest) error {
	if req.ParentID < 1 || req.Generation == 0 || req.OwnerEpoch == 0 || req.ScannerThrough < 0 {
		return ErrInvalidLease
	}
	m.mu.Lock()
	p := m.entryLocked(req.ParentID)
	m.mu.Unlock()
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrLeaseRequired
	}
	if req.OwnerEpoch < p.record.OwnerEpoch || req.Generation < p.record.Generation {
		m.mu.Unlock()
		return ErrLeaseConflict
	}
	if req.OwnerEpoch == p.record.OwnerEpoch && !p.record.Rejoining {
		m.mu.Unlock()
		return nil
	}
	if req.OwnerEpoch > p.record.OwnerEpoch {
		m.closeLocked(p)
		p.pending = nil
		p.record = leaseRecord{ParentID: req.ParentID, Generation: req.Generation, Required: true, Retired: true, Rejoining: true, OwnerEpoch: req.OwnerEpoch}
	}
	row := p.record
	m.mu.Unlock()
	if err := m.persist(row); err != nil {
		return err
	}
	if m.quiesce != nil {
		if err := m.quiesce(ctx, req.ParentID); err != nil {
			return err
		}
	}
	if m.rejoinCleanup != nil {
		if err := m.rejoinCleanup(ctx, req.ParentID, req.OwnerEpoch); err != nil {
			return err
		}
	}
	if req.ScannerEpoch != nil {
		if m.rejoinScanner == nil {
			return ErrLeaseConflict
		}
		if err := m.rejoinScanner(ctx, req); err != nil {
			return err
		}
	}
	row.Retired, row.Rejoining = false, false
	if err := m.persist(row); err != nil {
		return err
	}
	m.mu.Lock()
	p.record, p.expired = row, true
	m.mu.Unlock()
	return nil
}
