package agentservice

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type comparisonSnapshot struct{ value storageconfig.Snapshot }

func (s comparisonSnapshot) Snapshot() (storageconfig.Snapshot, error) { return s.value, nil }

func (s *Service) comparisonCommand(ctx context.Context, c agenttransport.Command) (agenttransport.Response, bool, error) {
	if c.Operation != "comparison_start" && c.Operation != "comparison_get" && c.Operation != "comparison_cancel" && c.Operation != "comparison_delete" {
		return agenttransport.Response{}, false, nil
	}
	reply := func(value any, err error) (agenttransport.Response, bool, error) {
		if errors.Is(err, comparison.ErrBusy) {
			r := agenttransport.Response{Status: 429, Payload: []byte(`{"message":"Another comparison is running on this node"}`)}
			return r, true, nil
		}
		if errors.Is(err, comparison.ErrNotFound) {
			r := agenttransport.Response{Status: 404, Payload: []byte(`{"message":"Comparison not found"}`)}
			return r, true, nil
		}
		r, e := response(value, err)
		return r, true, e
	}
	if s.Comparisons == nil {
		return reply(nil, errors.New("Agent does not support range comparisons; update this node"))
	}
	if c.Operation != "comparison_get" && !c.Mutating {
		r, e := badRequest(errors.New("operation requires a mutating command"))
		return r, true, e
	}
	if c.Operation == "comparison_start" {
		var request comparison.Request
		if err := decode(c.Payload, &request); err != nil {
			return reply(nil, err)
		}
		if err := comparison.Validate(request); err != nil {
			r, e := badRequest(err)
			return r, true, e
		}
		snapshot, err := s.Configs.Snapshot()
		if err != nil {
			return reply(nil, err)
		}
		if request.StorageRevision == 0 || snapshot.Revision != request.StorageRevision {
			return reply(nil, storageconfig.ErrStaleRevision)
		}
		// Freeze the connection settings in memory before the asynchronous job
		// starts. Only its revision and aliases are persisted in the request.
		factory := *s.Factory
		factory.Source = comparisonSnapshot{snapshot}
		p, err := s.Comparisons.Start(request, &factory)
		return reply(p, err)
	}
	var request struct {
		ID    string `json:"id"`
		After int64  `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := decode(c.Payload, &request); err != nil {
		return reply(nil, err)
	}
	if c.Operation == "comparison_cancel" {
		p, err := s.Comparisons.Cancel(request.ID)
		return reply(p, err)
	}
	if c.Operation == "comparison_delete" {
		return reply(map[string]bool{"success": true}, s.Comparisons.Delete(request.ID))
	}
	p, err := s.Comparisons.Get(request.ID, request.After, request.Limit)
	return reply(p, err)
}
