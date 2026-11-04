package agentservice

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
)

// ChildPollProtocol is advertised in children_get responses. A Master that
// sees it may use children_poll, which answers notModified for records whose
// stored fingerprint it already holds. Older Agents omit it, and older Masters
// ignore it and the per-row fingerprints.
const ChildPollProtocol = 1

type ChildRecordsRequest struct {
	ChildIDs []string `json:"childIds"`
}

// ChildPollItem names a child and, optionally, the fingerprint of the record
// the Master already stored for it.
type ChildPollItem struct {
	ChildID     string `json:"childId"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
type ChildPollRequest struct {
	Children []ChildPollItem `json:"children"`
}
type ChildRecord struct {
	ChildID string            `json:"childId"`
	Record  *execution.Record `json:"record,omitempty"`
	Missing bool              `json:"missing,omitempty"`
	// NotModified (children_poll only) means the stored record still has the
	// fingerprint the Master sent; Record is omitted.
	NotModified bool `json:"notModified,omitempty"`
	// Fingerprint identifies the stored record version, when not Missing.
	Fingerprint string `json:"fingerprint,omitempty"`
}
type ChildRecordsResponse struct {
	Children     []ChildRecord `json:"children"`
	PollProtocol int           `json:"pollProtocol,omitempty"`
}

func (s *Service) childRecords(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	var request ChildRecordsRequest
	if err := decode(command.Payload, &request); err != nil {
		return badRequest(err)
	}
	items := make([]ChildPollItem, len(request.ChildIDs))
	for i, id := range request.ChildIDs {
		items[i].ChildID = id
	}
	return s.childRecordPage(ctx, items)
}

func (s *Service) childPoll(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	var request ChildPollRequest
	if err := decode(command.Payload, &request); err != nil {
		return badRequest(err)
	}
	for _, item := range request.Children {
		if len(item.Fingerprint) > 128 {
			return badRequest(errors.New("invalid child record fingerprint"))
		}
	}
	return s.childRecordPage(ctx, request.Children)
}

func (s *Service) childRecordPage(ctx context.Context, items []ChildPollItem) (agenttransport.Response, error) {
	if len(items) == 0 || len(items) > 16 {
		return badRequest(errors.New("child record page must contain 1 to 16 IDs"))
	}
	responsePage := ChildRecordsResponse{Children: make([]ChildRecord, 0, len(items)), PollProtocol: ChildPollProtocol}
	size := 64
	for _, item := range items {
		id := item.ChildID
		if id == "" || len(id) > 256 {
			return badRequest(errors.New("invalid child ID"))
		}
		record, fingerprint, notModified, err := s.Engine.GetIfChanged(ctx, id, item.Fingerprint)
		row := ChildRecord{ChildID: id}
		if errors.Is(err, execution.ErrNotFound) {
			row.Missing = true
		} else if err != nil {
			return response(nil, err)
		} else {
			row.Fingerprint, row.NotModified = fingerprint, notModified
			if !notModified {
				row.Record = &record
			}
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return response(nil, err)
		}
		if size+len(raw)+1 > 4<<20 {
			if len(responsePage.Children) == 0 {
				return agenttransport.Response{Status: 413, Payload: json.RawMessage(`{"code":"batch_item_too_large"}`)}, nil
			}
			break
		}
		responsePage.Children = append(responsePage.Children, row)
		size += len(raw) + 1
	}
	return response(responsePage, nil)
}
