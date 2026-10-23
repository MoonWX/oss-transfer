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

func (t *taskRuntime) observeNode(ctx context.Context, p task.Parent, children []task.Child, stopOnError bool) ([]task.Observation, error) {
	var observations []task.Observation
	var problems []error
	legacy := false
	for offset := 0; offset < len(children); {
		if err := ctx.Err(); err != nil {
			return observations, errors.Join(append(problems, err)...)
		}
		group := children[offset:min(offset+16, len(children))]
		var rows []agentservice.ChildRecord
		if !legacy && len(group) > 1 {
			request := agentservice.ChildRecordsRequest{}
			for _, child := range group {
				request.ChildIDs = append(request.ChildIDs, child.Job.ChildID)
			}
			var page agentservice.ChildRecordsResponse
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := t.server.agentCall(callCtx, nodeSession(group[0].Node), "children_get", false, request, &page)
			cancel()
			if batchReadFallback(err) {
				legacy = true
			} else if err != nil {
				return observations, errors.Join(append(problems, err)...)
			} else {
				if len(page.Children) == 0 || len(page.Children) > len(group) {
					return observations, task.ErrConflict
				}
				for i, row := range page.Children {
					if row.ChildID != group[i].Job.ChildID || row.Missing == (row.Record != nil) || row.Record != nil && row.Record.Job.ChildID != row.ChildID {
						return observations, task.ErrConflict
					}
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
			record, err := t.observeOne(ctx, p, child, row)
			if err != nil {
				problems = append(problems, err)
				if stopOnError || ctx.Err() != nil {
					return observations, errors.Join(problems...)
				}
			} else if record != nil {
				observations = append(observations, task.Observation{Number: child.Job.Number, Record: *record})
			}
		}
		offset += len(group)
	}
	return observations, errors.Join(append(problems, ctx.Err())...)
}

func (t *taskRuntime) observeOne(ctx context.Context, p task.Parent, child task.Child, row *agentservice.ChildRecord) (*execution.Record, error) {
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
	if !agentStatus(err, 404) {
		if err != nil {
			return nil, err
		}
		return &record, nil
	}
	if p.Status != "RUNNING" {
		return nil, nil
	}
	err = t.server.agentCall(callCtx, nodeSession(child.Node), "child_accept", true, agentservice.ChildAcceptRequest{Job: child.Job, InventorySource: p.Spec.InventoryID != ""}, nil)
	var agentErr *AgentError
	if child.Job.SharedLocalSource && errors.As(err, &agentErr) && agentErr.Message == agentservice.ErrSharedVerificationRequired.Error() {
		return nil, nil
	}
	return nil, err
}
