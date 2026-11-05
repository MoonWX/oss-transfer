package master

import (
	"context"
	"errors"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// A requested node change passes through stages that look like faults from a
// single reconciliation step: the leaving node refuses work once it has been
// fenced, an offline leaving node is waited out until its lease expires, and
// the scan pauses while it moves to another node. These stages are reported as
// membership progress in the node view, never as the task's runtime problem.
// Unexpected disconnects and failing nodes are still reported.

// leasePeerRetired reports an Agent reply saying that its task lease has been
// permanently retired.
func leasePeerRetired(err error) bool {
	if errors.Is(err, agentservice.ErrLeaseRetired) {
		return true
	}
	var remote *AgentError
	return errors.As(err, &remote) && remote.Status == 409 && remote.Message == agentservice.ErrLeaseRetired.Error()
}

// plannedLeaving reports a member that leaves because the user asked for it:
// it is draining, being removed immediately, or already removed.
func plannedLeaving(p task.Parent, nodeID string) bool {
	if nodeID == "" {
		return false
	}
	_, draining := p.NodeDraining(nodeID)
	return draining || p.NodeRemoved(nodeID)
}

// plannedRetirementError reports a refusal from a node that this task retired
// on purpose. It is the expected answer of a fenced node.
func plannedRetirementError(p task.Parent, err error) bool {
	var remote *AgentError
	return leasePeerRetired(err) && errors.As(err, &remote) && plannedLeaving(p, remote.NodeID)
}

// withoutPlannedProblems drops the parts of a step error, joined or not, that
// describe an expected stage of a requested node change.
func withoutPlannedProblems(p task.Parent, err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, part := range joined.Unwrap() {
			if part = withoutPlannedProblems(p, part); part != nil {
				kept = append(kept, part)
			}
		}
		return errors.Join(kept...)
	}
	if plannedRetirementError(p, err) {
		return nil
	}
	return err
}

// runtimeProblemMessage stores one problem per line, without repeats, so the
// console can show and translate each separately.
func runtimeProblemMessage(err error) string {
	if err == nil {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// recordStep runs one reconciliation step and saves what it could not do as
// the task's runtime problem. Expected stages of a requested node change are
// left out; the node view reports them as progress.
func (t *taskRuntime) recordStep(ctx context.Context, id int64, generation uint64) error {
	err := t.step(ctx, id)
	if ctx.Err() != nil {
		return err
	}
	message := ""
	if err != nil && !isStopAwaitingAcknowledgement(err) {
		if current, readErr := t.repo.Get(id); readErr == nil {
			err = withoutPlannedProblems(current, err)
		}
		message = runtimeProblemMessage(err)
	}
	_ = t.repo.RuntimeProblem(id, generation, message)
	return err
}

// Scan phases shown in the node view.
const (
	scanPhaseScanning = "SCANNING" // the scanner lists the source or still serves batches
	scanPhaseHandoff  = "HANDOFF"  // a new scanner is rebuilding from the Master's checkpoint
	scanPhaseWaiting  = "WAITING"  // the scanner has been fenced; another node takes over next
	scanPhaseComplete = "COMPLETE" // every scanned batch has been assigned; no scanner is needed
)

// scanFinished reports a completed scan whose batches have all been assigned.
// The scanner is no longer needed after this point.
func scanFinished(p task.Parent) bool {
	return p.ScanComplete && p.DispatchedThrough >= p.ScannedBatches && p.Status != "RESUMING"
}

// scanPhase describes where the scan stands. Only SCANNING and HANDOFF have a
// member that is the current scanner.
func scanPhase(p task.Parent) string {
	switch {
	case scanFinished(p):
		return scanPhaseComplete
	case p.Failover != nil && p.Failover.ScannerSeeding:
		return scanPhaseHandoff
	case p.NodeRetired(p.ScannerNode().ID):
		return scanPhaseWaiting
	}
	return scanPhaseScanning
}

// plannedScannerHandoff reports a scanner that is unavailable because the user
// asked to remove it. The scan moves to another node after the scanner has
// been fenced (or its lease has expired); this is not a reconnect wait.
func plannedScannerHandoff(p task.Parent) bool {
	return plannedLeaving(p, p.ScannerNode().ID)
}
