package task

import "github.com/MoonWX/oss-transfer/internal/discovery"

// A scanner may run far ahead of dispatch. Only retain a complete source page
// whose entire output is included in the same or an earlier assignment commit.
func retainScanCheckpoint(p *Parent, checkpoint *discovery.RecoveryCheckpoint) error {
	if checkpoint == nil {
		return nil
	}
	if !checkpoint.Valid() || checkpoint.Epoch != p.ScannerEpoch() || checkpoint.ThroughBatch > int64(p.DispatchedThrough) ||
		checkpoint.Objects > p.DispatchedObjects || checkpoint.Bytes > p.DispatchedBytes {
		return ErrConflict
	}
	if prior := p.ScanCheckpoint; prior != nil {
		if checkpoint.ThroughBatch < prior.ThroughBatch || checkpoint.Epoch < prior.Epoch || checkpoint.Epoch == prior.Epoch && checkpoint.Sequence < prior.Sequence {
			return nil
		}
		if checkpoint.Epoch == prior.Epoch && checkpoint.Sequence == prior.Sequence {
			// The same page. Its scan protocol requirement is not part of its
			// identity: an Agent or Master that predates the field drops it.
			// The stricter requirement is kept.
			same := *checkpoint
			same.RequiresScanProtocol = prior.RequiresScanProtocol
			if same != *prior {
				return ErrConflict
			}
			if checkpoint.RequiresScanProtocol <= prior.RequiresScanProtocol {
				return nil
			}
		}
		if checkpoint.Objects < prior.Objects || checkpoint.Bytes < prior.Bytes {
			return ErrConflict
		}
	}
	value := *checkpoint
	p.ScanCheckpoint = &value
	return nil
}
