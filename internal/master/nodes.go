package master

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
)

func installNodeRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/worker-status", s.adminOnly(s.workerStatus))
	s.mux.HandleFunc("GET /api/master/rate-limit", s.adminOnly(s.workerStatus))
	s.mux.HandleFunc("POST /api/master/rate-limit/{type}", s.adminOnly(s.setNodeRate))
	s.mux.HandleFunc("POST /api/master/rate-limit/{type}/{workerIp}", s.adminOnly(s.setNodeRate))
	s.mux.HandleFunc("GET /api/master/rate-limit/history", s.adminOnly(s.nodeRateHistory))
	s.mux.HandleFunc("GET /api/master/rate-limit/adjustment", s.adminOnly(s.nodeRateDetail))
}

type nodeReply struct {
	Member cluster.Member
	Value  map[string]any
	Err    error
}

// A missing Agent stays visible. Requests share one deadline and bounded fanout,
// so one offline Agent cannot hold a page open for a full timeout per node.
func (s *Server) nodeReplies(ctx context.Context, operation string) ([]nodeReply, error) {
	members, err := s.agents.registry.Members()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := make([]nodeReply, len(members))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for index, member := range members {
		wg.Add(1)
		go func(index int, member cluster.Member) {
			defer wg.Done()
			out[index].Member = member
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				out[index].Err = ctx.Err()
				return
			}
			out[index].Err = s.agentCall(ctx, member.Session(), operation, false, struct{}{}, &out[index].Value)
			if out[index].Err == nil {
				s.rememberNodeInfo(member.ID, out[index].Value)
			}
		}(index, member)
	}
	wg.Wait()
	return out, nil
}

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	replies, err := s.nodeReplies(r.Context(), "node_status")
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	displays := s.nodeDisplaySnapshot()
	workers := map[string]any{}
	missing := []string{}
	for _, reply := range replies {
		if reply.Err != nil {
			workers[reply.Member.Address] = map[string]any{"success": false, "message": reply.Err.Error()}
			missing = append(missing, reply.Member.Address)
		} else {
			workers[reply.Member.Address] = reply.Value
		}
		value := workers[reply.Member.Address].(map[string]any)
		if value == nil {
			value = map[string]any{"success": false, "message": "Agent returned no node status"}
			workers[reply.Member.Address] = value
			missing = append(missing, reply.Member.Address)
		}
		value["id"], value["address"], value["label"] = reply.Member.ID, reply.Member.Address, reply.Member.Label
		addNodeDisplay(value, displays.lookup(reply.Member.ID, reply.Member.Address))
	}
	sort.Strings(missing)
	writeJSON(w, 200, map[string]any{"success": true, "workers": workers, "nodeDisplays": displays.byAddress, "complete": len(missing) == 0, "partial": len(missing) > 0, "failedWorkers": missing})
}

func (s *Server) localNodesWithSettings(w http.ResponseWriter, r *http.Request) {
	replies, err := s.nodeReplies(r.Context(), "node_config")
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	displays := s.nodeDisplaySnapshot()
	nodes := make([]map[string]any, 0, len(replies))
	for _, reply := range replies {
		s.agents.mu.Lock()
		remote := s.agents.remoteAddress[reply.Member.ID]
		s.agents.mu.Unlock()
		node := map[string]any{"executionNodeIp": reply.Member.Address, "id": reply.Member.ID, "label": reply.Member.Label, "connectionAddress": remote, "reachable": reply.Err == nil, "localTransferSupported": true, "agent": true}
		if reply.Err != nil {
			node["message"] = reply.Err.Error()
		} else {
			node["advertisedLocalRoots"] = reply.Value["advertisedLocalRoots"]
			node["hostname"] = reply.Value["hostname"]
			node["localIps"] = reply.Value["localIps"]
		}
		addNodeDisplay(node, displays.lookup(reply.Member.ID, reply.Member.Address))
		nodes = append(nodes, node)
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"nodes": nodes}})
}

func (s *Server) nodeByAddress(address string) (cluster.Member, error) {
	members, err := s.agents.registry.Members()
	if err != nil {
		return cluster.Member{}, err
	}
	for _, member := range members {
		if member.Address == address {
			return member, nil
		}
	}
	return cluster.Member{}, nodecontrol.ErrNotFound
}
func nodeHTTPError(w http.ResponseWriter, err error) {
	status := 500
	var remote *AgentError
	switch {
	case errors.Is(err, nodecontrol.ErrNotFound):
		status = 404
	case errors.Is(err, nodecontrol.ErrInvalid):
		status = 400
	case errors.As(err, &remote):
		status = remote.Status
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status = 503
	}
	fail(w, status, err.Error())
}
func (s *Server) updateAgentNode(w http.ResponseWriter, r *http.Request) {
	var change struct {
		Address string    `json:"address"`
		Label   *string   `json:"label"`
		Roots   *[]string `json:"advertisedLocalRoots"`
	}
	if !readJSON(w, r, &change) {
		return
	}
	member, err := s.nodeByAddress(r.PathValue("address"))
	if err != nil {
		nodeHTTPError(w, err)
		return
	}
	if change.Address != "" && change.Address != member.Address {
		fail(w, 409, "An enrolled Agent address cannot be changed; enroll the replacement Agent instead")
		return
	}
	if change.Label != nil && (!utf8.ValidString(*change.Label) || len(*change.Label) > 128 || strings.ContainsAny(*change.Label, "\x00\r\n")) {
		fail(w, 400, "Invalid node label")
		return
	}
	if change.Roots != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.agentCall(ctx, member.Session(), "node_config_set", true, nodecontrol.Config{AdvertisedLocalRoots: *change.Roots}, nil); err != nil {
			nodeHTTPError(w, err)
			return
		}
	}
	if change.Label != nil {
		if _, err := s.agents.registry.UpdateLabel(member.ID, *change.Label); err != nil {
			if change.Roots != nil {
				fail(w, 500, "Local roots were saved, but the node label could not be saved")
			} else {
				fail(w, 500, "Cannot save node label")
			}
			return
		}
	}
	data, err := s.agentNodeData()
	if err != nil {
		fail(w, 500, "Node settings were saved, but the node list could not be read")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}

func (s *Server) setNodeRate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		QPS *float64 `json:"qps"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	kind := r.PathValue("type")
	if request.QPS == nil || math.IsNaN(*request.QPS) || math.IsInf(*request.QPS, 0) || *request.QPS < 0 {
		fail(w, 400, "Invalid rate")
		return
	}
	value := *request.QPS
	if kind == "bandwidth" {
		if value > 1e12 || value > 0 && value < 8 {
			fail(w, 400, "Invalid bandwidth")
			return
		}
	} else if kind != "list" && kind != "put" && kind != "head" || value > 1e6 || value > 0 && value < .01 {
		fail(w, 400, "Invalid request rate")
		return
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	address := r.PathValue("workerIp")
	if address != "" {
		member, err := s.nodeByAddress(address)
		if err != nil {
			nodeHTTPError(w, err)
			return
		}
		members = []cluster.Member{member}
	}
	if len(members) == 0 {
		fail(w, 409, "No Agent is registered")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	displays := s.nodeDisplaySnapshot()
	results := make([]map[string]any, len(members))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for index, member := range members {
		wg.Add(1)
		go func(index int, member cluster.Member) {
			defer wg.Done()
			result := map[string]any{"workerIp": member.Address, "success": false}
			addNodeDisplay(result, displays.lookup(member.ID, member.Address))
			results[index] = result
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result["message"] = ctx.Err().Error()
				return
			}
			var rates map[string]any
			if err := s.agentCall(ctx, member.Session(), "node_rate_set", true, map[string]any{"type": kind, "qps": value}, &rates); err != nil {
				result["message"] = err.Error()
				return
			}
			result["success"], result["rateLimit"] = true, rates
		}(index, member)
	}
	wg.Wait()
	good := 0
	for _, result := range results {
		if result["success"] == true {
			good++
		}
	}
	message := "Node rate updated"
	if good != len(members) {
		message = "Some Agents did not confirm the rate update"
	}
	writeJSON(w, 200, map[string]any{"success": good == len(members), "successCount": good, "failedCount": len(members) - good, "results": results, "nodeDisplays": displays.byAddress, "message": message})
}

type workerAdjustment struct {
	nodecontrol.Adjustment
	NodeDisplay
	WorkerIP string `json:"workerIp"`
}

func (s *Server) nodeRateHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			fail(w, 400, "History limit must be between 1 and 200")
			return
		}
		limit = value
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	slots := make(chan struct{}, 8)
	displays := s.nodeDisplaySnapshot()
	adjustments := []workerAdjustment{}
	missing := []string{}
	for _, member := range members {
		wg.Add(1)
		go func(member cluster.Member) {
			defer wg.Done()
			var entries []nodecontrol.Adjustment
			var err error
			select {
			case slots <- struct{}{}:
				err = s.agentCall(ctx, member.Session(), "node_rate_history", false, map[string]int{"limit": limit}, &entries)
				<-slots
			case <-ctx.Done():
				err = ctx.Err()
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				missing = append(missing, member.Address)
				return
			}
			for _, entry := range entries {
				adjustments = append(adjustments, workerAdjustment{Adjustment: entry, NodeDisplay: displays.lookup(member.ID, member.Address), WorkerIP: member.Address})
			}
		}(member)
	}
	wg.Wait()
	sort.SliceStable(adjustments, func(i, j int) bool { return adjustments[i].Timestamp > adjustments[j].Timestamp })
	if len(adjustments) > limit {
		adjustments = adjustments[:limit]
	}
	sort.Strings(missing)
	writeJSON(w, 200, map[string]any{"success": true, "adjustments": adjustments, "nodeDisplays": displays.byAddress, "partial": len(missing) > 0, "complete": len(missing) == 0, "failedWorkers": missing})
}
func (s *Server) nodeRateDetail(w http.ResponseWriter, r *http.Request) {
	member, err := s.nodeByAddress(r.URL.Query().Get("workerIp"))
	if err != nil {
		nodeHTTPError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var detail nodecontrol.Detail
	if err := s.agentCall(ctx, member.Session(), "node_rate_detail", false, map[string]string{"eventId": r.URL.Query().Get("eventId"), "cursor": r.URL.Query().Get("cursor")}, &detail); err != nil {
		nodeHTTPError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		nodecontrol.Detail
		NodeDisplay
		Success  bool   `json:"success"`
		WorkerIP string `json:"workerIp"`
	}{detail, s.nodeDisplaySnapshot().lookup(member.ID, member.Address), true, member.Address})
}
