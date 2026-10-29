package agentservice

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// ParentCleanupProgress reports committed records and stages, never a guessed
// percentage. Polling repeats admission safely; cleanup itself never needs polls.
type ParentCleanupProgress struct {
	Done           bool   `json:"done"`
	Phase          string `json:"phase"`
	RecordsDeleted int64  `json:"recordsDeleted"`
	Error          string `json:"error,omitempty"`
}

func (s *Service) cleanupParent(ctx context.Context, request ParentRequest) (ParentCleanupProgress, error) {
	if request.ParentID < 1 || request.Generation == 0 {
		return ParentCleanupProgress{}, discovery.ErrInvalid
	}
	// Validate both fences before changing either side; a stale delete must not
	// retire execution merely because only discovery has advanced its generation.
	current, err := s.Jobs.Parent(ctx, request.ParentID)
	if err != nil && !errors.Is(err, execution.ErrNotFound) {
		return ParentCleanupProgress{}, err
	}
	if err == nil && request.Generation < current.Generation {
		return ParentCleanupProgress{}, execution.ErrGeneration
	}
	scan, err := s.Scanner.Status(ctx, request.ParentID)
	if err != nil && !errors.Is(err, discovery.ErrNotFound) {
		return ParentCleanupProgress{}, err
	}
	if err == nil && request.Generation < scan.Generation {
		return ParentCleanupProgress{}, discovery.ErrGeneration
	}
	if err = s.store.Update(func(tx *state.Tx) error { return state.RequireSchema(tx, "agent", state.SchemaBackgroundCleanup) }); err != nil {
		return ParentCleanupProgress{}, err
	}
	if err = s.Engine.RequestDeleteParent(ctx, request.ParentID, request.Generation); err != nil {
		return ParentCleanupProgress{}, err
	}
	if err = s.Scanner.RequestDeleteParent(ctx, request.ParentID, request.Generation); err != nil {
		return ParentCleanupProgress{}, err
	}
	executionProgress, err := s.Engine.DeletionProgress(ctx, request.ParentID, request.Generation)
	if err != nil {
		return ParentCleanupProgress{}, err
	}
	scan, err = s.Scanner.Status(ctx, request.ParentID)
	if err != nil {
		return ParentCleanupProgress{}, err
	}
	result := ParentCleanupProgress{RecordsDeleted: executionProgress.RecordsDeleted + scan.CleanupDeleted, Error: executionProgress.Error, Phase: "EXECUTION"}
	if result.Error == "" {
		result.Error = scan.CleanupError
	}
	if executionProgress.Done {
		result.Phase = "DISCOVERY"
	}
	result.Done = executionProgress.Done && scan.Purged
	if result.Done {
		result.Phase = "DONE"
		result.Error = ""
		s.Shared.Forget(request.ParentID, 0)
	}
	return result, nil
}
