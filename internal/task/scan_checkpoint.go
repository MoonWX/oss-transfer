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
		if checkpoint.Epoch == prior.Epoch && checkpoint.Sequence == prior.Sequence && *checkpoint != *prior {
			return ErrConflict
		}
		if checkpoint.Objects < prior.Objects || checkpoint.Bytes < prior.Bytes {
			return ErrConflict
		}
	}
	value := *checkpoint
	p.ScanCheckpoint = &value
	return nil
}
