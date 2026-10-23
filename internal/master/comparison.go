package master

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type comparisonAssignment struct {
	Request      comparison.Request `json:"request"`
	NodeID       string             `json:"nodeId"`
	EnrollmentID string             `json:"enrollmentId"`
}

func (s *Server) startComparison(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SourceBucket      string `json:"sourceBucket"`
		DestinationBucket string `json:"destBucket"`
		SourcePrefix      string `json:"sourcePrefix"`
		DestinationPrefix string `json:"destPrefix"`
		Mode              string `json:"mode"`
		Scope             string `json:"scope"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	a, err := comparisonEndpoint(input.SourceBucket, snapshot)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	b, err := comparisonEndpoint(input.DestinationBucket, snapshot)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	id, err := randomToken()
	if err != nil {
		fail(w, 500, "Cannot create comparison")
		return
	}
	request := comparison.Request{ID: id, StorageRevision: snapshot.Revision, Source: a, Destination: b, SourcePrefix: input.SourcePrefix, DestinationPrefix: input.DestinationPrefix, Mode: input.Mode, Scope: input.Scope}
	if err = comparison.Validate(request); err != nil {
		fail(w, 400, err.Error())
		return
	}
	nodes, err := s.onlineAgents()
	if err != nil {
		fail(w, 500, "Cannot read Agent state")
		return
	}
	if len(nodes) == 0 {
		fail(w, 503, "No connected Agent is available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	for _, node := range nodes {
		if err = s.syncStorage(ctx, node, snapshot); err != nil {
			continue
		}
		assignment := comparisonAssignment{request, node.ID, node.EnrollmentID}
		if err = s.store.Update(func(tx *state.Tx) error { return tx.Put("comparison_assignments", id, assignment) }); err != nil {
			fail(w, 500, "Cannot save comparison")
			return
		}
		var progress comparison.Progress
		err = s.agentCall(ctx, node.Session(), "comparison_start", true, request, &progress)
		var problem *AgentError
		unsupported := errors.As(err, &problem) && (problem.Status == 400 || problem.Status == 404) && (strings.Contains(strings.ToLower(problem.Message), "unknown operation") || strings.Contains(strings.ToLower(problem.Message), "unknown_operation") || strings.Contains(strings.ToLower(problem.Message), "unsupported operation"))
		if agentStatus(err, 429) || unsupported {
			// These replies explicitly reject admission. Do not leave a mapping
			// to work which was never started, even if every Agent is busy.
			if err := s.forgetComparison(id); err != nil {
				fail(w, 500, "Cannot save comparison")
				return
			}
			continue
		}
		if err != nil {
			// The durable assignment remains discoverable after an uncertain
			// response; never start the same comparison on another node.
			writeJSON(w, 502, map[string]any{"success": false, "id": id, "message": redactStorageError(err, snapshot.Connections).Error()})
			return
		}
		writeJSON(w, 202, progress)
		return
	}
	fail(w, 503, "No Agent is available for comparison; a comparison may already be running")
}

func (s *Server) forgetComparison(id string) error {
	return s.store.Update(func(tx *state.Tx) error { return tx.Delete("comparison_assignments", id) })
}

func (s *Server) deleteComparison(w http.ResponseWriter, r *http.Request) {
	var result map[string]any
	if !s.comparisonCall(w, r, "comparison_delete", map[string]any{"id": r.PathValue("id")}, &result) {
		return
	}
	if err := s.forgetComparison(r.PathValue("id")); err != nil {
		fail(w, 500, "Cannot save comparison")
		return
	}
	writeJSON(w, 200, map[string]bool{"success": true})
}

func (s *Server) comparisonSession(id string) (agenttransport.Session, error) {
	var a comparisonAssignment
	err := s.store.View(func(tx *state.Tx) error {
		found, err := tx.Get("comparison_assignments", id, &a)
		if err != nil {
			return err
		}
		if !found {
			return comparison.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return agenttransport.Session{}, err
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		return agenttransport.Session{}, err
	}
	for _, member := range members {
		if member.ID == a.NodeID && member.EnrollmentID == a.EnrollmentID {
			return member.Session(), nil
		}
	}
	return agenttransport.Session{}, errors.New("The comparison node has been removed")
}

func (s *Server) comparisonCall(w http.ResponseWriter, r *http.Request, operation string, request any, into any) bool {
	session, err := s.comparisonSession(r.PathValue("id"))
	if err != nil {
		status := 503
		if errors.Is(err, comparison.ErrNotFound) {
			status = 404
		}
		fail(w, status, err.Error())
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err = s.agentCall(ctx, session, operation, operation != "comparison_get", request, into); err != nil {
		status := 502
		if agentStatus(err, 404) {
			status = 404
			var problem *AgentError
			if errors.As(err, &problem) && problem.Message == "Comparison not found" {
				// The Agent has deleted or expired this result. Network errors and
				// unsupported operations must retain their assignment for recovery.
				if deleteErr := s.forgetComparison(r.PathValue("id")); deleteErr != nil {
					fail(w, 500, "Cannot save comparison")
					return false
				}
			}
		}
		fail(w, status, err.Error())
		return false
	}
	return true
}
func (s *Server) getComparison(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	limit := 50
	var err error
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			fail(w, 400, "Invalid comparison cursor")
			return
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			fail(w, 400, "Comparison page size must be between 1 and 200")
			return
		}
	}
	var result comparison.Page
	if s.comparisonCall(w, r, "comparison_get", map[string]any{"id": r.PathValue("id"), "after": after, "limit": limit}, &result) {
		result.Error = localize(w, result.Error)
		for i := range result.Items {
			result.Items[i].Error = localize(w, result.Items[i].Error)
		}
		writeJSON(w, 200, result)
	}
}
func (s *Server) cancelComparison(w http.ResponseWriter, r *http.Request) {
	var result comparison.Progress
	if s.comparisonCall(w, r, "comparison_cancel", map[string]any{"id": r.PathValue("id")}, &result) {
		writeJSON(w, 200, result)
	}
}
