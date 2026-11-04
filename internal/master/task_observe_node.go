package master

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func batchReadFallback(err error) bool {
	var problem *AgentError
	return errors.As(err, &problem) && (problem.Status == 404 && problem.Message == "unknown_operation" || problem.Status == 413 && problem.Message == "batch_item_too_large")
}

// nodeObservation is one node's observe result. fingerprints holds the stored
// record fingerprint for observations read in full from an Agent that reports
// them; notModified lists children whose record matched the fingerprint sent.
type nodeObservation struct {
	observations []task.Observation
	fingerprints map[int]string
	notModified  []int
}

func pollFallback(err error) bool {
	var problem *AgentError
	return errors.As(err, &problem) && problem.Status == 404 && problem.Message == "unknown_operation"
}

// observeNode reads records in pages of 16. Agents advertising
// agentservice.ChildPollProtocol are read with children_poll, carrying the
// fingerprints in known; others (and a downgraded Agent, which answers
// unknown_operation) use children_get, and very old Agents child_get.
func (t *taskRuntime) observeNode(ctx context.Context, p task.Parent, children []task.Child, known map[int]string, stopOnError bool) (nodeObservation, error) {
	result := nodeObservation{fingerprints: map[int]string{}}
	var problems []error
	legacy := false
	poll := len(children) > 0 && t.nodePolls(children[0].Node)
	for offset := 0; offset < len(children); {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(problems, err)...)
		}
		group := children[offset:min(offset+16, len(children))]
		var rows []agentservice.ChildRecord
		// A single child also uses the batch read: its response advertises
		// children_poll support, and that matters most for tasks with few
		// long-running children per node.
		if !legacy {
			var page agentservice.ChildRecordsResponse
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var err error
			if poll {
				request := agentservice.ChildPollRequest{}
				for _, child := range group {
					request.Children = append(request.Children, agentservice.ChildPollItem{ChildID: child.Job.ChildID, Fingerprint: known[child.Job.Number]})
				}
				err = t.server.agentCall(callCtx, nodeSession(group[0].Node), "children_poll", false, request, &page)
			} else {
				request := agentservice.ChildRecordsRequest{}
				for _, child := range group {
					request.ChildIDs = append(request.ChildIDs, child.Job.ChildID)
				}
				err = t.server.agentCall(callCtx, nodeSession(group[0].Node), "children_get", false, request, &page)
			}
			cancel()
			if poll && pollFallback(err) {
				// The Agent was downgraded; read this page with children_get.
				t.setNodePolls(group[0].Node, false)
				poll = false
				continue
			} else if batchReadFallback(err) {
				legacy = true
			} else if err != nil {
				return result, errors.Join(append(problems, err)...)
			} else {
				if len(page.Children) == 0 || len(page.Children) > len(group) {
					return result, task.ErrConflict
				}
				for i, row := range page.Children {
					sent := known[group[i].Job.Number]
					outcomes := 0
					for _, set := range []bool{row.Missing, row.NotModified, row.Record != nil} {
						if set {
							outcomes++
						}
					}
					if row.ChildID != group[i].Job.ChildID || outcomes != 1 || row.Record != nil && row.Record.Job.ChildID != row.ChildID || row.NotModified && (!poll || sent == "" || row.Fingerprint != sent) {
						return result, task.ErrConflict
					}
				}
				if !poll && page.PollProtocol >= agentservice.ChildPollProtocol {
					t.setNodePolls(group[0].Node, true)
				}
				rows = page.Children
				group = group[:len(rows)]
			}
		}
		for index, child := range group {
			var row *agentservice.ChildRecord
			if rows != nil {
				row = &rows[index]
			}
			if row != nil && row.NotModified {
				result.notModified = append(result.notModified, child.Job.Number)
				continue
			}
			record, err := t.observeOne(ctx, p, child, row)
			if err != nil {
				problems = append(problems, err)
				if stopOnError || ctx.Err() != nil {
					return result, errors.Join(problems...)
				}
			} else if record != nil {
				result.observations = append(result.observations, task.Observation{Number: child.Job.Number, Record: *record})
				if row != nil && row.Fingerprint != "" {
					result.fingerprints[child.Job.Number] = row.Fingerprint
				}
			}
		}
		offset += len(group)
	}
	return result, errors.Join(append(problems, ctx.Err())...)
}

func (t *taskRuntime) observeOne(ctx context.Context, p task.Parent, child task.Child, row *agentservice.ChildRecord) (*execution.Record, error) {
	if p.HandoffPending(child.Job.Number) {
		return nil, nil
	}
	var record execution.Record
	var err error
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if row == nil {
		err = t.server.agentCall(callCtx, nodeSession(child.Node), "child_get", false, map[string]string{"childId": child.Job.ChildID}, &record)
	} else if row.Missing {
		err = &AgentError{Status: 404}
	} else {
		record = *row.Record
	}
	if err == nil && (record.Job.ChildID != child.Job.ChildID || record.Job.Generation == 0) {
		return nil, task.ErrConflict
	}
	if !agentStatus(err, 404) && !(err == nil && (record.Job.Generation < p.Generation || record.Job.Assignment < child.Job.Assignment)) {
		if err != nil {
			return nil, err
		}
		return &record, nil
	}
	if p.Status != "RUNNING" {
		return nil, nil
	}
	if child.Job.Items == nil {
		// The observe loop reads children without item lists; redelivery
		// needs the complete assignment.
		full, err := t.repo.Child(p.ID, child.Job.Number)
		if err != nil {
			return nil, err
		}
		if full.Job.ChildID != child.Job.ChildID || full.Job.Assignment != child.Job.Assignment || full.Node.ID != child.Node.ID {
			// Reassigned since the page was read; the next pass sees it.
			return nil, nil
		}
		child = full
	}
	err = t.server.agentCall(callCtx, nodeSession(child.Node), "child_accept", true, childAcceptance(p, child), nil)
	var agentErr *AgentError
	if child.Job.SharedLocalSource && errors.As(err, &agentErr) && agentErr.Message == agentservice.ErrSharedVerificationRequired.Error() {
		return nil, nil
	}
	return nil, err
}
