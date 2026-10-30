package master

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

func installExceptionRoutes(s *Server) error {
	if err := s.tasks.repo.EnsureExceptionIndex(); err != nil {
		return err
	}
	s.mux.HandleFunc("POST /api/master/failed", s.adminOnly(s.listExceptions(false)))
	s.mux.HandleFunc("POST /api/master/fallback", s.adminOnly(s.listExceptions(true)))
	s.mux.HandleFunc("POST /api/master/failed/clear", s.adminOnly(s.clearExceptions(false)))
	s.mux.HandleFunc("POST /api/master/fallback/clear", s.adminOnly(s.clearExceptions(true)))
	s.mux.HandleFunc("GET /api/master/local-transfers/failures", s.adminOnly(s.localExceptions))
	s.mux.HandleFunc("GET /api/master/exceptions/tasks", s.adminOnly(s.exceptionTasks))
	return nil
}

type exceptionRequest struct {
	Type            string `json:"type"`
	TaskIndex       int64  `json:"taskIndex"`
	TaskID          int64  `json:"taskId"`
	WorkerNode      string `json:"workerNode"`
	ExecutionNodeIP string `json:"executionNodeIp"`
	Direction       string `json:"direction"`
	SourceBucket    string `json:"sourceBucket"`
	DestBucket      string `json:"destBucket"`
	Endpoint        string `json:"endpoint"`
	Limit           int    `json:"limit"`
}

func exceptionBody(w http.ResponseWriter, r *http.Request) (task.ExceptionFilter, int, bool) {
	request := exceptionRequest{Limit: 1000}
	if r.Body != nil && r.ContentLength != 0 {
		if !readJSON(w, r, &request) {
			return task.ExceptionFilter{}, 0, false
		}
	}
	if request.TaskIndex < 0 || request.TaskID < 0 || request.TaskID > 0 && request.TaskIndex > 0 && request.TaskID != request.TaskIndex || request.Limit < 1 || request.Limit > 10000 {
		fail(w, 400, "Invalid exception query")
		return task.ExceptionFilter{}, 0, false
	}
	if request.Type != "" && request.Type != "failed" && request.Type != "local" {
		fail(w, 400, "Invalid exception query")
		return task.ExceptionFilter{}, 0, false
	}
	node, err := mergeTaskString(request.WorkerNode, request.ExecutionNodeIP, "execution node")
	if err != nil {
		fail(w, 400, err.Error())
		return task.ExceptionFilter{}, 0, false
	}
	if request.Direction != "" && request.Direction != "OSS_TO_OSS" && request.Direction != "LOCAL_TO_OSS" && request.Direction != "OSS_TO_LOCAL" {
		fail(w, 400, "Invalid transfer direction")
		return task.ExceptionFilter{}, 0, false
	}
	return task.ExceptionFilter{ParentID: max(request.TaskIndex, request.TaskID), Node: node, Direction: request.Direction, SourceBucket: request.SourceBucket, DestinationBucket: request.DestBucket, Endpoint: request.Endpoint, LocalOnly: request.Type == "local"}, request.Limit, true
}
func exceptionDTO(e task.Exception) map[string]any {
	local := ""
	if e.Source.Kind == "LOCAL" {
		local = joinAgentPath(e.Source.Root, e.Key)
	} else if e.Destination.Kind == "LOCAL" {
		local = joinAgentPath(e.Destination.Root, e.DestinationKey)
	}
	status := e.RetryStatus
	if status == "" {
		status = task.ExceptionNotRetried
	}
	result := map[string]any{"taskIndex": e.ParentID, "parentTaskId": e.ParentID, "childTaskId": e.ChildID, "direction": e.Direction, "sourceS3Alias": e.Source.Ref, "sourceBucket": e.Source.Bucket, "destS3Alias": e.Destination.Ref, "destBucket": e.Destination.Bucket, "key": e.Key, "logicalKey": e.Key, "destinationKey": e.DestinationKey, "localPath": local, "objectSize": e.Size, "workerNode": e.Node, "executionNodeIp": e.Node, "stage": "TRANSFER", "errorCode": "TRANSFER_FAILED", "reasonCode": "TRANSFER_FAILED", "errorMessage": e.Error, "retryCount": e.RetryCount, "retryStatus": status, "timestamp": e.ObservedAt.UnixMilli(), "timestampFormatted": e.ObservedAt.UTC().Format(time.RFC3339), "observedAt": e.ObservedAt.UnixMilli(), "targetCommitted": e.TargetCommitted}
	if e.RetryGeneration > 0 {
		result["retryGeneration"] = e.RetryGeneration
	}
	if !e.RetryUpdatedAt.IsZero() {
		result["retryUpdatedAt"] = e.RetryUpdatedAt.UnixMilli()
	}
	if e.RetryError != "" {
		result["retryError"] = e.RetryError
	}
	if e.RetryNode != "" {
		result["retryExecutionNodeIp"] = e.RetryNode
	}
	return result
}

// The index retains failed objects and their latest observed retry outcomes.
// A disconnected Agent may have newer results, so cached evidence remains visible.
func (s *Server) exceptionNodes(filter task.ExceptionFilter) (map[string]bool, error) {
	members, err := s.agents.registry.Members()
	if err != nil {
		return nil, err
	}
	online, err := s.onlineAgents()
	if err != nil {
		return nil, err
	}
	connected := map[string]bool{}
	for _, member := range online {
		connected[member.Address] = true
	}
	result := map[string]bool{}
	if filter.ParentID > 0 {
		p, err := s.tasks.repo.Get(filter.ParentID)
		if err != nil && !errors.Is(err, task.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			for _, node := range p.Spec.Nodes {
				if filter.Node == "" || node.Address == filter.Node {
					result[node.Address] = connected[node.Address]
				}
			}
		}
		return result, nil
	}
	for _, member := range members {
		if filter.Node == "" || member.Address == filter.Node {
			result[member.Address] = connected[member.Address]
		}
	}
	if filter.Node != "" {
		if _, found := result[filter.Node]; !found {
			result[filter.Node] = false
		}
	}
	return result, nil
}
func exceptionNodeLists(nodes map[string]bool) (queried, failed []string) {
	queried, failed = []string{}, []string{}
	for node, online := range nodes {
		queried = append(queried, node)
		if !online {
			failed = append(failed, node)
		}
	}
	sort.Strings(queried)
	sort.Strings(failed)
	return
}

func (s *Server) listExceptions(fallback bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, limit, ok := exceptionBody(w, r)
		if !ok {
			return
		}
		nodes, err := s.exceptionNodes(filter)
		if err != nil {
			fail(w, 500, "Cannot read exception nodes")
			return
		}
		displays := s.nodeDisplaySnapshot()
		groups := map[string]any{}
		counts := map[string]int64{}
		var clearable int64
		for node := range nodes {
			groups[node] = []map[string]any{}
		}
		if !fallback {
			err = s.tasks.repo.VisitExceptions(r.Context(), filter, func(e task.Exception) error {
				if e.Resolved() {
					clearable++
				}
				rows, found := groups[e.Node].([]map[string]any)
				if !found {
					rows = []map[string]any{}
					nodes[e.Node] = false
				}
				counts[e.Node]++
				if len(rows) < limit {
					row := exceptionDTO(e)
					addExecutionNodeDisplay(row, displays.lookup("", e.Node))
					if e.RetryNode != "" {
						row["retryExecutionNodeDisplay"] = displays.lookup("", e.RetryNode)
					}
					rows = append(rows, row)
				}
				groups[e.Node] = rows
				return nil
			})
			if err != nil {
				fail(w, 500, "Cannot read failure ledger")
				return
			}
		}
		var total int64
		truncated := false
		for node := range nodes {
			groups[node+"Count"] = counts[node]
			total += counts[node]
			truncated = truncated || counts[node] > int64(limit)
		}
		queried, failed := exceptionNodeLists(nodes)
		name := "failedKeys"
		if fallback {
			name = "fallbackObjects"
			failed = []string{}
		}
		result := map[string]any{"success": true, name: groups, "totalCount": total, "clearableCount": clearable, "nodeDisplays": displays.byAddress, "queriedWorkers": queried, "failedWorkers": failed, "unreachableWorkers": failed, "complete": len(failed) == 0, "partial": len(failed) > 0, "truncated": truncated, "workerErrors": map[string]string{}}
		if fallback {
			result["supported"] = false
			result["message"] = localize(w, "The Go transfer engine does not use the legacy SDK fallback mechanism")
		}
		writeJSON(w, 200, result)
	}
}
func (s *Server) clearExceptions(fallback bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, _, ok := exceptionBody(w, r)
		if !ok {
			return
		}
		if fallback {
			writeJSON(w, 200, map[string]any{"success": true, "complete": true, "partial": false, "clearedCount": 0, "supported": false, "failedWorkers": []string{}})
			return
		}
		count, err := s.tasks.repo.ClearExceptions(r.Context(), filter)
		if err != nil {
			fail(w, 500, "Cannot clear displayed exceptions")
			return
		}
		// Dismissal is committed by the Master alone. An offline Agent cannot
		// make that local transaction incomplete or require a repeated clear.
		writeJSON(w, 200, map[string]any{"success": true, "clearedCount": count, "nodeDisplays": s.nodeDisplaySnapshot().byAddress, "complete": true, "partial": false, "queriedWorkers": []string{}, "failedWorkers": []string{}, "unreachableWorkers": []string{}, "scope": "resolved-exceptions", "executionLedgerPreserved": true})
	}
}
func (s *Server) localExceptions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := task.ExceptionFilter{LocalOnly: true, Node: q.Get("executionNodeIp"), Direction: q.Get("direction"), Endpoint: q.Get("endpoint")}
	var err error
	if raw := q.Get("taskId"); raw != "" {
		filter.ParentID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.ParentID < 0 {
			fail(w, 400, "Invalid task ID")
			return
		}
	}
	if filter.Direction != "" && filter.Direction != "LOCAL_TO_OSS" && filter.Direction != "OSS_TO_LOCAL" {
		fail(w, 400, "Invalid local direction")
		return
	}
	if q.Get("storageId") != "" {
		fail(w, 400, "Shared storage IDs are not supported; filter by execution node or endpoint")
		return
	}
	offset, limit := 0, 25
	if raw := q.Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			fail(w, 400, "Invalid offset")
			return
		}
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			fail(w, 400, "Limit must be 1 to 1000")
			return
		}
	}
	nodes, err := s.exceptionNodes(filter)
	if err != nil {
		fail(w, 500, "Cannot read exception nodes")
		return
	}
	items := []map[string]any{}
	total := 0
	clearable := 0
	displays := s.nodeDisplaySnapshot()
	err = s.tasks.repo.VisitExceptions(r.Context(), filter, func(e task.Exception) error {
		if e.Resolved() {
			clearable++
		}
		if _, found := nodes[e.Node]; !found {
			nodes[e.Node] = false
		}
		if total >= offset && len(items) < limit {
			row := exceptionDTO(e)
			addExecutionNodeDisplay(row, displays.lookup("", e.Node))
			if e.RetryNode != "" {
				row["retryExecutionNodeDisplay"] = displays.lookup("", e.RetryNode)
			}
			items = append(items, row)
		}
		total++
		return nil
	})
	if err != nil {
		fail(w, 500, "Cannot read failure ledger")
		return
	}
	queried, failed := exceptionNodeLists(nodes)
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"items": items, "nodeDisplays": displays.byAddress, "total": total, "clearableCount": clearable, "offset": offset, "limit": limit, "hasMore": offset+len(items) < total, "partial": len(failed) > 0, "complete": len(failed) == 0, "queriedWorkers": queried, "unreachableWorkers": failed, "failedWorkers": failed}})
}

func (s *Server) exceptionTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("type")
	if kind == "" {
		kind = "failed"
	}
	if kind != "failed" && kind != "local" && kind != "fallback" {
		fail(w, 400, "Invalid exception query")
		return
	}
	filter := task.ExceptionFilter{LocalOnly: kind == "local"}
	if search := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(q.Get("search")), "#")); search != "" {
		id, err := strconv.ParseInt(search, 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "Invalid task ID")
			return
		}
		filter.ParentID = id
	}
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(w, 400, "Invalid exception query")
			return
		}
		limit = parsed
	}
	items := []map[string]any{}
	hasMore := false
	if kind != "fallback" {
		parents, more, err := s.tasks.repo.ExceptionTasks(r.Context(), filter, limit)
		if err != nil {
			fail(w, 500, "Cannot read failure ledger")
			return
		}
		hasMore = more
		for _, p := range parents {
			items = append(items, taskDTO(p))
		}
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"items": items, "hasMore": hasMore}})
}
