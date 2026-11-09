package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type comparisonSnapshot struct{ value storageconfig.Snapshot }

func (s comparisonSnapshot) Snapshot() (storageconfig.Snapshot, error) { return s.value, nil }

// errComparisonStorageChanged leaves an interrupted comparison for a manual
// resume when its storage connections changed while the Agent was stopped.
var errComparisonStorageChanged = errors.New("storage connections changed; resume manually")

// comparisonFactory freezes the connection settings of snapshot in memory
// before an asynchronous comparison starts. Only the revision and aliases are
// persisted with the request.
func comparisonFactory(factory *storageconfig.Factory, snapshot storageconfig.Snapshot) execution.StoreFactory {
	frozen := *factory
	frozen.Source = comparisonSnapshot{snapshot}
	return revisionFactory{Factory: &frozen, revision: snapshot.Revision}
}

// revisionFactory tells the comparison Manager which storage revision a
// resumed comparison runs with.
type revisionFactory struct {
	*storageconfig.Factory
	revision uint64
}

func (f revisionFactory) StorageRevision() uint64 { return f.revision }

// comparisonOptions completes the Agent's comparison settings: an interrupted
// comparison resumes automatically only with the storage revision it was
// started with, and failures are written to the service log.
func comparisonOptions(options comparison.Options, configs *storageconfig.Agent, factory *storageconfig.Factory) comparison.Options {
	if options.ResumeFactory == nil {
		options.ResumeFactory = func(r comparison.Request) (execution.StoreFactory, error) {
			snapshot, err := configs.Snapshot()
			if err != nil {
				return nil, err
			}
			if r.StorageRevision == 0 || snapshot.Revision != r.StorageRevision {
				return nil, errComparisonStorageChanged
			}
			return comparisonFactory(factory, snapshot), nil
		}
	}
	if options.Logf == nil {
		// os.Stderr is read at call time: the service host redirects it to the
		// service log after start-up.
		options.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
		}
	}
	return options
}

// comparisonListBudget keeps a comparison_list reply well inside the 8 MiB
// Agent channel payload limit.
const comparisonListBudget = 6 << 20

// fitComparisonList drops the per-item fields a list does not show (the
// checkpoint, last stored key and error detail, each up to a few KiB once
// JSON-escaped) when hundreds of comparisons with long keys would not fit in
// one reply. Every comparison stays listed: the Master forgets comparisons an
// Agent no longer lists.
func fitComparisonList(items []comparison.Progress) comparison.List {
	list := comparison.List{Items: items}
	if encoded, err := json.Marshal(list); err != nil || len(encoded) <= comparisonListBudget {
		return list
	}
	trimmed := make([]comparison.Progress, len(items))
	for i, p := range items {
		p.Checkpoint, p.LastStoredKey, p.ErrorDetail = "", "", ""
		trimmed[i] = p
	}
	return comparison.List{Items: trimmed}
}

// comparisonRequest is the payload of comparison_get, comparison_cancel and
// comparison_delete.
type comparisonRequest struct {
	ID    string `json:"id"`
	After int64  `json:"after"`
	Limit int    `json:"limit"`
}

func (s *Service) comparisonCommand(ctx context.Context, c agenttransport.Command) (agenttransport.Response, bool, error) {
	readOnly := false
	switch c.Operation {
	case "comparison_get", "comparison_rows", "comparison_list":
		readOnly = true
	case "comparison_start", "comparison_cancel", "comparison_delete", "comparison_resume":
	default:
		return agenttransport.Response{}, false, nil
	}
	reply := func(value any, err error) (agenttransport.Response, bool, error) {
		message := ""
		status := 0
		switch {
		case errors.Is(err, comparison.ErrBusy):
			status, message = 429, "Another comparison is running on this node"
		case errors.Is(err, comparison.ErrNotFound):
			status, message = 404, "Comparison not found"
		case errors.Is(err, comparison.ErrRunning):
			status, message = 409, "Comparison is running; cancel it before deleting"
		case errors.Is(err, comparison.ErrNotResumable):
			status, message = 409, "Comparison cannot be resumed"
		case errors.Is(err, comparison.ErrInvalidQuery):
			r, e := badRequest(err)
			return r, true, e
		}
		if status != 0 {
			return agenttransport.Response{Status: status, Payload: []byte(`{"message":"` + message + `"}`)}, true, nil
		}
		// Replies never reveal the Agent's local paths.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = fmt.Errorf("%s comparison results: %w", pathErr.Op, pathErr.Err)
		}
		r, e := response(value, err)
		return r, true, e
	}
	invalid := func(err error) (agenttransport.Response, bool, error) {
		r, e := badRequest(err)
		return r, true, e
	}
	if s.Comparisons == nil {
		return reply(nil, errors.New("Agent does not support range comparisons; update this node"))
	}
	if !readOnly && !c.Mutating {
		return invalid(errors.New("operation requires a mutating command"))
	}
	switch c.Operation {
	case "comparison_start":
		var request comparison.Request
		if err := decode(c.Payload, &request); err != nil {
			return invalid(err)
		}
		if err := comparison.Validate(request); err != nil {
			return invalid(err)
		}
		snapshot, err := s.Configs.Snapshot()
		if err != nil {
			return reply(nil, err)
		}
		if request.StorageRevision == 0 || snapshot.Revision != request.StorageRevision {
			return reply(nil, storageconfig.ErrStaleRevision)
		}
		p, err := s.Comparisons.Start(request, comparisonFactory(s.Factory, snapshot))
		return reply(p, err)
	case "comparison_resume":
		var request comparison.ResumeRequest
		if err := decode(c.Payload, &request); err != nil {
			return invalid(err)
		}
		if request.ID == "" {
			return invalid(errors.New("comparison ID is required"))
		}
		snapshot, err := s.Configs.Snapshot()
		if err != nil {
			return reply(nil, err)
		}
		if request.StorageRevision == 0 || snapshot.Revision != request.StorageRevision {
			return reply(nil, storageconfig.ErrStaleRevision)
		}
		p, err := s.Comparisons.Resume(request.ID, comparisonFactory(s.Factory, snapshot))
		return reply(p, err)
	case "comparison_rows":
		var query comparison.RowsQuery
		if err := decode(c.Payload, &query); err != nil {
			return invalid(err)
		}
		page, err := s.Comparisons.Rows(query)
		return reply(page, err)
	case "comparison_list":
		if len(c.Payload) > 0 {
			var request struct{}
			if err := decode(c.Payload, &request); err != nil {
				return invalid(err)
			}
		}
		items, err := s.Comparisons.List()
		return reply(fitComparisonList(items), err)
	}
	var request comparisonRequest
	if err := decode(c.Payload, &request); err != nil {
		return invalid(err)
	}
	switch c.Operation {
	case "comparison_cancel":
		p, err := s.Comparisons.Cancel(request.ID)
		return reply(p, err)
	case "comparison_delete":
		return reply(map[string]bool{"success": true}, s.Comparisons.Delete(request.ID))
	}
	page, err := s.Comparisons.Get(request.ID, request.After, request.Limit)
	return reply(page, err)
}
