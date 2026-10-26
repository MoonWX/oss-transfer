package task

import (
	"errors"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func Historical(p Parent) bool { return p.Legacy != nil && !p.Legacy.RecoveryStarted }

// ReadyLegacyRecovery records completion of the offline manifest, not a claim
// that any external object has been checked or that a task has resumed.
func (r *Repository) ReadyLegacyRecovery(id int64, migrationID string) error {
	return r.store.Update(func(tx *state.Tx) error {
		if err := requireMigration(tx, migrationID); err != nil {
			return err
		}
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Legacy == nil || p.Legacy.MigrationID != migrationID {
			return ErrConflict
		}
		p.Legacy.RecoveryReady = true
		return r.save(tx, p)
	})
}

// BeginLegacyRecovery is the one-time switch from historical counters to a new
// execution generation. The original parent remains in a separate history row.
// Retrying an uncertain response returns the same generation without resetting
// newly observed counters or resurrecting a subsequent stop.
func (r *Repository) BeginLegacyRecovery(id int64, resolved Spec) (Parent, error) {
	p, _, err := r.RequestLegacyRecovery(id, resolved, nil)
	return p, err
}

func (r *Repository) RequestLegacyRecovery(id int64, resolved Spec, expected *uint64) (p Parent, accepted bool, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Deleted || p.Purged {
			return ErrState
		}
		if expected != nil && p.Generation != *expected {
			return nil
		}
		if p.Legacy == nil || !p.Legacy.RecoveryReady {
			return ErrLegacyRecovery
		}
		if p.Legacy.RecoveryStarted {
			return nil
		}
		if !p.Legacy.NeedsReconciliation || p.Deleted || p.Purged || !Ended(p.Status) {
			return ErrState
		}
		for _, blocker := range p.Legacy.Blockers {
			if !RecoveryBlockerSupported(blocker) {
				return errors.New("legacy recovery blocked: " + blocker)
			}
		}
		original := p.Spec
		// Probe resolution can identify a local source file and its per-node parent
		// directory; it cannot change destinations, scope or conflict handling.
		checked := resolved
		checked.SharedLocalSource = original.SharedLocalSource
		if original.Source.Kind == "LOCAL" {
			checked.Source = original.Source
			checked.ObjectKey = original.ObjectKey
			if len(checked.Nodes) != len(original.Nodes) {
				return ErrConflict
			}
			checked.Nodes = append([]Node(nil), checked.Nodes...)
			for i := range checked.Nodes {
				checked.Nodes[i].LocalRoot = original.Nodes[i].LocalRoot
			}
		}
		if !reflect.DeepEqual(checked, original) {
			return ErrConflict
		}
		if err := validate(resolved); err != nil {
			return err
		}
		if p.Generation == ^uint64(0) || p.BudgetRevision == ^uint64(0) {
			return ErrGeneration
		}
		if err := tx.Put("master-task-legacy-original", key(id), p); err != nil {
			return err
		}
		p.Spec = resolved
		p.Fingerprint = digest(resolved)
		p.Generation++
		p.BudgetRevision++
		p.Status = "RESUMING"
		p.ScannedObjects = 0
		p.ScannedBytes = 0
		p.ScanComplete = false
		p.ScannedBatches = 0
		p.DispatchedThrough = 0
		p.DispatchedObjects = 0
		p.DispatchedBytes = 0
		p.ChildCount = 0
		p.FinishedChildren = 0
		p.FailedChildren = 0
		p.Counters = execution.Counters{}
		p.Error = ""
		p.RuntimeError = ""
		now := r.now().UTC()
		p.StartedAt = &now
		p.ActiveTime = &execution.ActiveTime{} // Earlier imported timing stays in the archived original.
		p.FinishedAt = nil
		p.Legacy.NeedsReconciliation = false
		p.Legacy.RecoveryStarted = true
		accepted = true
		return r.save(tx, p)
	})
	return
}

func RecoveryBlockerSupported(blocker string) bool {
	switch blocker {
	case "LOCAL_SOURCE_KIND_REQUIRES_PROBE", "SHARED_LOCAL_SOURCE_REQUIRES_VERIFICATION", "DISCOVERY_REQUIRES_RECONCILIATION", "OBJECT_HISTORY_REQUIRES_RECONCILIATION":
		return true
	default:
		return false
	}
}
