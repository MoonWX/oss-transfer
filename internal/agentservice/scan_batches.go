package agentservice

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
)

type ScanBatchesRequest struct {
	ScanRequest
	Limit int `json:"limit"`
}

type ScanBatchesResponse struct {
	Batches []discovery.Batch `json:"batches"`
}

// A page changes neither scanner acknowledgement nor assignment ownership.
// Keep payloads below the broker's limit even when object metadata is large.
func (s *Service) scanBatches(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	var request ScanBatchesRequest
	if err := decode(command.Payload, &request); err != nil {
		return badRequest(err)
	}
	if request.Limit < 1 || request.Limit > 16 {
		return badRequest(errors.New("scan batch limit must be between 1 and 16"))
	}
	result := ScanBatchesResponse{Batches: make([]discovery.Batch, 0, request.Limit)}
	size := 32
	for range request.Limit {
		batch, found, err := s.Scanner.Batch(ctx, request.ParentID, request.Generation, request.Number)
		if err != nil {
			return response(nil, err)
		}
		if !found {
			break
		}
		raw, err := json.Marshal(batch)
		if err != nil {
			return response(nil, err)
		}
		if size+len(raw)+1 > 4<<20 {
			if len(result.Batches) == 0 {
				return agenttransport.Response{Status: 413, Payload: json.RawMessage(`{"code":"batch_item_too_large"}`)}, nil
			}
			break
		}
		result.Batches = append(result.Batches, batch)
		size += len(raw) + 1
		request.Number = batch.Number
	}
	return response(result, nil)
}
