package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type AgentError struct {
	Status  int
	Message string
}

func (e *AgentError) Error() string { return e.Message }

// agentCall carries only commands and metadata. In particular it must not turn
// the Master into a fallback S3 client when no Agent is available.
func (s *Server) agentCall(ctx context.Context, session agenttransport.Session, operation string, mutation bool, value any, into any) error {
	requestID, err := cluster.Token()
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := s.agents.broker.Call(ctx, session, agenttransport.Request{RequestID: requestID, Operation: operation, Mutating: mutation, IdempotencyKey: requestID, Payload: data})
	if err != nil {
		return err
	}
	if response.Status < 200 || response.Status >= 300 {
		var problem struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		_ = json.Unmarshal(response.Payload, &problem)
		if problem.Message == "" {
			problem.Message = problem.Code
		}
		if problem.Message == "" {
			problem.Message = fmt.Sprintf("Agent returned HTTP %d", response.Status)
		}
		return &AgentError{response.Status, problem.Message}
	}
	if into != nil {
		return json.Unmarshal(response.Payload, into)
	}
	return nil
}
func (s *Server) onlineAgents() ([]cluster.Member, error) {
	members, err := s.agents.registry.Members()
	if err != nil {
		return nil, err
	}
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	var result []cluster.Member
	for _, member := range members {
		seen, ok := s.agents.lastSeen[member.ID]
		if ok && s.options.Now().Sub(seen) < 75*time.Second {
			result = append(result, member)
		}
	}
	return result, nil
}
func (s *Server) syncStorage(ctx context.Context, member cluster.Member, snapshot storageconfig.Snapshot) error {
	var result struct {
		Revision uint64 `json:"revision"`
	}
	if err := s.agentCall(ctx, member.Session(), "storage_apply", true, snapshot, &result); err != nil {
		return err
	}
	if result.Revision != snapshot.Revision {
		return errors.New("Agent acknowledged a different storage revision")
	}
	return nil
}
func installStorageRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/s3-config", s.adminOnly(s.listStorage))
	s.mux.HandleFunc("POST /api/master/s3-config", s.adminOnly(s.saveStorage))
	s.mux.HandleFunc("PUT /api/master/s3-config/{alias}", s.adminOnly(s.saveStorage))
	s.mux.HandleFunc("DELETE /api/master/s3-config/{alias}", s.adminOnly(s.deleteStorage))
	s.mux.HandleFunc("GET /api/master/s3-config/{alias}/buckets", s.adminOnly(s.listStorageBuckets))
}
func (s *Server) listStorage(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "configs": snapshot.Connections, "revision": snapshot.Revision})
}
func (s *Server) storageBuckets(ctx context.Context, config storageconfig.Config) ([]string, error) {
	members, err := s.onlineAgents()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, errors.New("no Agent is online; join an Agent before validating storage")
	}
	var result struct {
		Buckets []string `json:"buckets"`
	}
	if err := s.agentCall(ctx, members[0].Session(), "storage_buckets", false, config, &result); err != nil {
		return nil, err
	}
	return result.Buckets, nil
}
func (s *Server) saveStorage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		storageconfig.Config
		Validate bool `json:"validate"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if err := storageconfig.Validate(request.Config); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if request.Validate {
		if _, err := s.storageBuckets(r.Context(), request.Config); err != nil {
			fail(w, 400, "Storage validation failed: "+err.Error())
			return
		}
	}
	configs := storageconfig.NewMaster(s.store)
	var snapshot storageconfig.Snapshot
	var err error
	if r.Method == "POST" {
		snapshot, err = configs.Create(request.Config)
	} else {
		snapshot, err = configs.Update(r.PathValue("alias"), request.Config)
	}
	if err != nil {
		storageError(w, err)
		return
	}
	s.storageSaved(w, r, snapshot)
}
func storageError(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, storageconfig.ErrNotFound):
		status = 404
	case errors.Is(err, storageconfig.ErrExists):
		status = 409
	case errors.Is(err, storageconfig.ErrInvalid):
		status = 400
	}
	fail(w, status, err.Error())
}
func (s *Server) deleteStorage(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Delete(r.PathValue("alias"))
	if err != nil {
		storageError(w, err)
		return
	}
	s.storageSaved(w, r, snapshot)
}
func (s *Server) storageSaved(w http.ResponseWriter, r *http.Request, snapshot storageconfig.Snapshot) {
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Connection saved; cannot read Agent synchronization status")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	details := make([]string, len(members))
	success := make([]bool, len(members))
	slots := make(chan struct{}, 8)
	var group sync.WaitGroup
	for i, member := range members {
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				details[i] = member.Address + ": 等待同步"
				return
			}
			if err := s.syncStorage(ctx, member, snapshot); err != nil {
				details[i] = member.Address + ": 等待同步"
			} else {
				success[i] = true
				details[i] = member.Address + ": 同步成功"
			}
		}()
	}
	group.Wait()
	count := 0
	for _, ok := range success {
		if ok {
			count++
		}
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "存储连接已保存", "revision": snapshot.Revision, "successCount": count, "failedCount": len(members) - count, "partial": count < len(members), "details": details})
}
func (s *Server) listStorageBuckets(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	for _, config := range snapshot.Connections {
		if config.Alias == r.PathValue("alias") {
			buckets, err := s.storageBuckets(r.Context(), config)
			if err != nil {
				fail(w, 502, err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"success": true, "buckets": buckets})
			return
		}
	}
	fail(w, 404, "Storage connection not found")
}
