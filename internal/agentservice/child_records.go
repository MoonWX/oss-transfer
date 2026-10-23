package agentservice

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
)

type ChildRecordsRequest struct {
	ChildIDs []string `json:"childIds"`
}
type ChildRecord struct {
	ChildID string            `json:"childId"`
	Record  *execution.Record `json:"record,omitempty"`
	Missing bool              `json:"missing,omitempty"`
}
type ChildRecordsResponse struct {
	Children []ChildRecord `json:"children"`
}

func (s *Service) childRecords(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	var request ChildRecordsRequest
	if err := decode(command.Payload, &request); err != nil {
		return badRequest(err)
	}
	if len(request.ChildIDs) == 0 || len(request.ChildIDs) > 16 {
		return badRequest(errors.New("child record page must contain 1 to 16 IDs"))
	}
	responsePage := ChildRecordsResponse{Children: make([]ChildRecord, 0, len(request.ChildIDs))}
	size := 32
	for _, id := range request.ChildIDs {
		if id == "" || len(id) > 256 {
			return badRequest(errors.New("invalid child ID"))
		}
		record, err := s.Engine.Get(ctx, id)
		row := ChildRecord{ChildID: id}
		if errors.Is(err, execution.ErrNotFound) {
			row.Missing = true
		} else if err != nil {
			return response(nil, err)
		} else {
			row.Record = &record
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
