package master

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Keep responses bounded: an Agent record can include a whole batch of objects.
const stopObservationPageSize = 8
const stopObservationPagesPerStep = 64

type stopNodeObservation struct {
	enrollment string
	epoch      uint64
	after      string
	done       bool
}

type stopObservation struct {
	generation uint64
	nodes      map[string]stopNodeObservation
}

func (t *taskRuntime) stopObservationCursor(p task.Parent, node task.Node) stopNodeObservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	progress := t.stopProgress[p.ID]
	if progress == nil || progress.generation != p.Generation {
		return stopNodeObservation{}
	}
	cursor := progress.nodes[node.ID]
	if cursor.enrollment != node.EnrollmentID || cursor.epoch != p.NodeEpoch(node.ID) {
		return stopNodeObservation{}
	}
	return cursor
}

func (t *taskRuntime) saveStopObservation(p task.Parent, node task.Node, cursor stopNodeObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopProgress == nil {
		t.stopProgress = map[int64]*stopObservation{}
	}
	progress := t.stopProgress[p.ID]
	if progress == nil || progress.generation != p.Generation {
		progress = &stopObservation{generation: p.Generation, nodes: map[string]stopNodeObservation{}}
		t.stopProgress[p.ID] = progress
	}
	cursor.enrollment = node.EnrollmentID
	cursor.epoch = p.NodeEpoch(node.ID)
	progress.nodes[node.ID] = cursor
}

func (t *taskRuntime) forgetStopObservation(id int64) {
	t.mu.Lock()
	delete(t.stopProgress, id)
	delete(t.stopWaits, id)
	t.mu.Unlock()
}

// Reconcile only after every stop fence has been acknowledged. Advance the
// cursor after persisting each page, so a slow page or a round's deadline cannot
// make a large task restart its entire ledger on every attempt. Cursors are
// disposable: after a Master restart, replaying already persisted observations
// is idempotent. STOPPING itself is durable until every node has been drained.
func (t *taskRuntime) observeStopped(ctx context.Context, p task.Parent) (bool, error) {
	pages := 0
	for _, node := range p.ActiveNodes() {
		cursor := t.stopObservationCursor(p, node)
		if cursor.done {
			continue
		}
		stopped, err := t.nodeLeaseStopped(p, node)
		if err != nil {
			return false, err
		}
		if stopped {
			cursor.done = true
			t.saveStopObservation(p, node, cursor)
			continue
		}
		for !cursor.done {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if pages >= stopObservationPagesPerStep {
				return false, nil
			}
			var rows []execution.Record
			if err := t.server.agentCall(ctx, nodeSession(node), "children", false, agentservice.ChildrenRequest{ParentID: p.ID, After: cursor.after, Limit: stopObservationPageSize}, &rows); err != nil {
				return false, err
			}
			observations := make([]task.Observation, 0, len(rows))
			for _, record := range rows {
				if record.ReleasedAssignment != nil {
					continue
				}
				if record.Job.OwnerEpoch != p.NodeEpoch(node.ID) {
					continue // Historical ownership on a rejoined node is no longer authoritative.
				}
				if record.Job.Generation != p.Generation {
					// Successful historical children are not rewritten for every retry.
					if record.Job.Generation < p.Generation && record.Status == execution.Completed {
						continue
					}
					return false, task.ErrGeneration
				}
				observations = append(observations, task.Observation{Number: record.Job.Number, Record: record})
			}
			if err := t.repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
				return false, err
			}
			cursor.done = len(rows) < stopObservationPageSize
			if len(rows) > 0 {
				next := rows[len(rows)-1].Job.ChildID
				if next == cursor.after {
					return false, errors.New("Agent child pagination did not advance")
				}
				cursor.after = next
			}
			t.saveStopObservation(p, node, cursor)
			pages++
		}
	}
	return true, nil
}
