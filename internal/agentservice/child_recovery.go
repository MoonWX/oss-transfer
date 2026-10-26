package agentservice

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

type ChildRecoverRequest struct {
	ParentID   int64  `json:"parentId"`
	ChildID    string `json:"childId"`
	Generation uint64 `json:"generation"`
}

type ChildRecoverResult struct {
	Outcome string           `json:"outcome"`
	Record  execution.Record `json:"record"`
}

func (s *Service) recoverChild(ctx context.Context, request ChildRecoverRequest) (ChildRecoverResult, error) {
	if err := s.Leases.Admission(request.ParentID, request.Generation); err != nil {
		return ChildRecoverResult{}, err
	}
	record, err := s.Engine.Get(ctx, request.ChildID)
	if err != nil {
		return ChildRecoverResult{}, err
	}
	if record.Job.ParentID != request.ParentID || record.Job.Generation != request.Generation {
		return ChildRecoverResult{}, execution.ErrGeneration
	}
	policy, err := s.Jobs.Parent(ctx, request.ParentID)
	if err != nil {
		return ChildRecoverResult{}, err
	}
	if policy.Generation != request.Generation {
		return ChildRecoverResult{}, execution.ErrGeneration
	}
	if policy.Stopped || policy.Deleted {
		return ChildRecoverResult{}, execution.ErrStopped
	}
	if record.Status == execution.Running {
		return ChildRecoverResult{Outcome: "ALREADY_RUNNING", Record: record}, nil
	}
	if record.Status != execution.Recoverable {
		return ChildRecoverResult{}, execution.ErrNotRecoverable
	}
	if !s.Shared.Allows(record.Job) {
		return ChildRecoverResult{}, ErrSharedVerificationRequired
	}
	outcome := "STARTED"
	err = s.Engine.TryRecover(ctx, request.ChildID, request.Generation)
	if errors.Is(err, execution.ErrCapacity) {
		// The durable ready index already contains this job. Leave normal
		// scheduling in charge; a click must not create an unbounded queue.
		outcome, err = "WAITING_CAPACITY", nil
	}
	if err != nil {
		return ChildRecoverResult{}, err
	}
	record, err = s.Engine.Get(ctx, request.ChildID)
	if err == nil && outcome == "WAITING_CAPACITY" && record.Status == execution.Running {
		outcome = "ALREADY_RUNNING"
	}
	return ChildRecoverResult{Outcome: outcome, Record: record}, err
}
