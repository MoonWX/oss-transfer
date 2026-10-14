package agentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// ReportRequest pages one immutable child's ledger. The Master supplies the
// expected generation and validates the returned immutable job digest.
type ReportRequest struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	ChildID    string `json:"childId"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
}
type ReportItem struct {
	Item   execution.Item       `json:"item"`
	Result execution.ItemResult `json:"result"`
}
type ReportPage struct {
	ParentID   int64            `json:"parentId"`
	Generation uint64           `json:"generation"`
	ChildID    string           `json:"childId"`
	JobDigest  string           `json:"jobDigest"`
	Status     execution.Status `json:"status"`
	Attempt    uint64           `json:"attempt"`
	ObservedAt time.Time        `json:"observedAt"`
	Total      int              `json:"total"`
	Offset     int              `json:"offset"`
	NextOffset int              `json:"nextOffset"`
	Done       bool             `json:"done"`
	Items      []ReportItem     `json:"items"`
}

func ReportJobDigest(job execution.Job) string {
	data, _ := json.Marshal(job)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *Service) Report(ctx context.Context, request ReportRequest) (ReportPage, error) {
	if request.ParentID < 1 || request.Generation == 0 || request.ChildID == "" || request.Offset < 0 || request.Limit < 1 || request.Limit > 128 {
		return ReportPage{}, errors.New("invalid ledger page request")
	}
	record, err := s.Jobs.Get(ctx, request.ChildID)
	if err != nil {
		return ReportPage{}, err
	}
	if record.Job.ParentID != request.ParentID || record.Job.Generation != request.Generation {
		return ReportPage{}, execution.ErrGeneration
	}
	if record.Status != execution.Completed && record.Status != execution.Failed && record.Status != execution.PartialFailed && record.Status != execution.Stopped {
		return ReportPage{}, execution.ErrRunning
	}
	// A stop fence is persisted before I/O cancellation. Wait for its old
	// attempt to exit before exposing a stable stopped ledger.
	record, err = s.Engine.Wait(ctx, request.ChildID)
	if err != nil {
		return ReportPage{}, err
	}
	if record.Job.Generation != request.Generation {
		return ReportPage{}, execution.ErrGeneration
	}
	if record.Status != execution.Completed && record.Status != execution.Failed && record.Status != execution.PartialFailed && record.Status != execution.Stopped {
		return ReportPage{}, execution.ErrRunning
	}
	if request.Offset > len(record.Job.Items) {
		return ReportPage{}, errors.New("ledger page offset exceeds item count")
	}
	results := make(map[string]execution.ItemResult, len(record.Results))
	for _, result := range record.Results {
		results[result.ID] = result
	}
	end := min(len(record.Job.Items), request.Offset+request.Limit)
	page := ReportPage{ParentID: request.ParentID, Generation: request.Generation, ChildID: request.ChildID, JobDigest: ReportJobDigest(record.Job), Status: record.Status, Attempt: record.Attempt, ObservedAt: record.UpdatedAt, Total: len(record.Job.Items), Offset: request.Offset, NextOffset: end, Done: end == len(record.Job.Items), Items: make([]ReportItem, 0, end-request.Offset)}
	for _, item := range record.Job.Items[request.Offset:end] {
		result, found := results[item.ID]
		if !found {
			return ReportPage{}, errors.New("object ledger entry is missing")
		}
		page.Items = append(page.Items, ReportItem{Item: item, Result: result})
	}
	return page, nil
}
