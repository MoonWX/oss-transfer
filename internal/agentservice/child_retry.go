package agentservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// RetryFailedProtocol is advertised by Agents that accept child_retry_failed:
// reopening the failed objects of ended children while their parent keeps
// running. The Master refuses that retry for Agents without it.
const RetryFailedProtocol = 1

// Outcomes of child_retry_failed for each requested child.
const (
	ChildRetryQueued    = "QUEUED"    // failed objects are pending again; the child waits for a slot
	ChildRetryUnchanged = "UNCHANGED" // not the observed ended attempt (a replay, or already reopened)
	ChildRetryCapacity  = "CAPACITY"  // the Agent's pending-job bound is reached
	ChildRetryRejected  = "REJECTED"  // fenced or unknown; see Error
)

type ChildRetryFailedRequest struct {
	ParentID   int64              `json:"parentId"`
	Generation uint64             `json:"generation"`
	Children   []ChildRetryTarget `json:"children"`
	// SourceRefresh says the Master accepts refreshed source pins. A Master
	// sends it only to Agents that advertise SourceRefreshProtocol.
	SourceRefresh bool `json:"sourceRefresh,omitempty"`
}

// ChildRetryTarget names a child and the ended attempt the Master observed.
type ChildRetryTarget struct {
	ChildID string `json:"childId"`
	Attempt uint64 `json:"attempt"`
}

type ChildRetryFailedResult struct {
	Children []ChildRetryOutcome `json:"children"`
}

type ChildRetryOutcome struct {
	ChildID string           `json:"childId"`
	Outcome string           `json:"outcome"`
	Status  execution.Status `json:"status,omitempty"`
	Attempt uint64           `json:"attempt,omitempty"`
	Error   string           `json:"error,omitempty"`
}

func (s *Service) retryFailedChildren(ctx context.Context, request ChildRetryFailedRequest) (ChildRetryFailedResult, error) {
	if request.ParentID < 1 || request.Generation == 0 || len(request.Children) == 0 || len(request.Children) > 16 {
		return ChildRetryFailedResult{}, errInvalidRetry
	}
	prefix := fmt.Sprintf("%d-", request.ParentID)
	for _, target := range request.Children {
		if !strings.HasPrefix(target.ChildID, prefix) || len(target.ChildID) > 64 || target.Attempt == 0 {
			return ChildRetryFailedResult{}, errInvalidRetry
		}
	}
	if err := s.Leases.Admission(request.ParentID, request.Generation); err != nil {
		return ChildRetryFailedResult{}, err
	}
	policy, err := s.Jobs.Parent(ctx, request.ParentID)
	if err != nil {
		return ChildRetryFailedResult{}, err
	}
	if policy.Generation != request.Generation {
		return ChildRetryFailedResult{}, execution.ErrGeneration
	}
	if policy.Stopped || policy.Deleted {
		return ChildRetryFailedResult{}, execution.ErrStopped
	}
	if err := s.Engine.SetSourceRefresh(ctx, request.ParentID, request.Generation, request.SourceRefresh); err != nil {
		return ChildRetryFailedResult{}, err
	}
	result := ChildRetryFailedResult{Children: make([]ChildRetryOutcome, 0, len(request.Children))}
	queued := false
	for _, target := range request.Children {
		// Each reopen checks the parent fence in its own transaction, so a
		// stop that arrives between children fences the rest of the batch.
		record, reopened, err := s.Engine.RetryFailed(ctx, target.ChildID, request.Generation, target.Attempt)
		outcome := ChildRetryOutcome{ChildID: target.ChildID, Status: record.Status, Attempt: record.Attempt}
		switch {
		case errors.Is(err, execution.ErrCapacity):
			outcome.Outcome = ChildRetryCapacity
		case err != nil:
			outcome.Outcome, outcome.Error = ChildRetryRejected, err.Error()
		case reopened:
			outcome.Outcome, queued = ChildRetryQueued, true
		default:
			outcome.Outcome = ChildRetryUnchanged
		}
		result.Children = append(result.Children, outcome)
	}
	if queued {
		s.signal()
	}
	return result, nil
}

var errInvalidRetry = errors.New("retry requires a parent, generation and 1 to 16 observed children")
