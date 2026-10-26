package agentservice

import (
	"context"
	"encoding/json"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
)

func (s *Service) scanTakeoverCommand(ctx context.Context, command agenttransport.Command) (agenttransport.Response, bool, error) {
	if command.Operation != "scan_takeover_seed" && command.Operation != "scan_takeover_finalize" {
		return agenttransport.Response{}, false, nil
	}
	if len(command.Payload) > discovery.MaxTakeoverPayloadBytes {
		return agenttransport.Response{Status: 413, Payload: json.RawMessage(`{"code":"takeover_page_too_large","message":"Scan takeover page exceeds 4 MiB"}`)}, true, nil
	}
	var value discovery.State
	var err error
	if command.Operation == "scan_takeover_seed" {
		var request discovery.TakeoverRequest
		if err = decode(command.Payload, &request); err != nil {
			result, responseErr := badRequest(err)
			return result, true, responseErr
		}
		if s.Leases != nil {
			err = s.Leases.Admission(request.Spec.ParentID, request.Spec.Generation)
		}
		if err == nil {
			value, err = s.Scanner.SeedTakeover(ctx, request)
		}
	} else {
		var request ScanRequest
		if err = decode(command.Payload, &request); err != nil {
			result, responseErr := badRequest(err)
			return result, true, responseErr
		}
		if s.Leases != nil {
			err = s.Leases.Admission(request.ParentID, request.Generation)
		}
		if err == nil {
			value, err = s.Scanner.FinalizeTakeover(ctx, request.ParentID, request.Generation, request.Epoch)
		}
	}
	result, responseErr := response(value, err)
	return result, true, responseErr
}
