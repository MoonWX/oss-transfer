package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type AgentError struct {
	Status  int
	Message string
	// NodeID identifies the Agent that replied, so a reply from a node the
	// task removed on purpose can be told apart from a real problem.
	NodeID string
}

func (e *AgentError) Error() string { return e.Message }

// agentCall carries only commands and metadata. In particular it must not turn
// the Master into a fallback S3 client when no Agent is available.
func (s *Server) agentCall(ctx context.Context, session agenttransport.Session, operation string, mutation bool, value any, into any) error {
	if softwareupdate.StartsWork(operation) && s.softwareUpdateBlocksNode(session.NodeID) {
		return &AgentError{Status: 409, Message: "Agent update is in progress"}
	}
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
		return &AgentError{Status: response.Status, Message: problem.Message, NodeID: session.NodeID}
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

// syncStorage applies the snapshot on one Agent. An Agent that predates
// automatic addressing reads an unset pathStyleEnable as virtual-hosted, so
// it receives the addressing that automatic resolves to (see
// storageAddressingSnapshot).
func (s *Server) syncStorage(ctx context.Context, member cluster.Member, snapshot storageconfig.Snapshot) error {
	resolved, differs := storageAddressingSnapshot(snapshot)
	if !differs {
		return s.applyStorage(ctx, member, snapshot)
	}
	automatic, err := s.addressing.supported(ctx, member, s.storageAddressingProtocol)
	if err != nil {
		return err
	}
	preferred, alternative := snapshot, resolved
	if !automatic {
		preferred, alternative = resolved, snapshot
	}
	err = s.applyStorage(ctx, member, preferred)
	// The Agent already holds this revision in the other form: a revision
	// that a v4.1 Master applied before the upgrade (unresolved), or one this
	// Master resolved while the Agent was old. Both forms are the same
	// connection to the Agent that holds it, and a revision never changes.
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Message == storageconfig.ErrRevisionConflict.Error() {
		if s.applyStorage(ctx, member, alternative) == nil {
			return nil
		}
	}
	return err
}

func (s *Server) applyStorage(ctx context.Context, member cluster.Member, snapshot storageconfig.Snapshot) error {
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

// storageAddressingSnapshot returns the snapshot as an Agent without
// automatic addressing must receive it: an unset choice that resolves to
// path-style is set explicitly. One that resolves to virtual-hosted stays
// unset, which such an Agent already reads as virtual-hosted. differs
// reports whether any connection changed.
func storageAddressingSnapshot(snapshot storageconfig.Snapshot) (resolved storageconfig.Snapshot, differs bool) {
	resolved = snapshot
	resolved.Connections = append([]storageconfig.Config(nil), snapshot.Connections...)
	for i, config := range resolved.Connections {
		resolved.Connections[i] = storageConfigForAgent(config)
		differs = differs || resolved.Connections[i].PathStyleEnable != config.PathStyleEnable
	}
	return resolved, differs
}

// storageConfigForAgent is one connection as an Agent without automatic
// addressing must receive it (see storageAddressingSnapshot).
func storageConfigForAgent(config storageconfig.Config) storageconfig.Config {
	if config.PathStyleEnable == nil && config.EffectivePathStyle() {
		pathStyle := true
		config.PathStyleEnable = &pathStyle
	}
	return config
}

func (s *Server) storageAddressingProtocol(ctx context.Context, member cluster.Member) (int, error) {
	var caps struct {
		StorageAddressingProtocol int `json:"storageAddressingProtocol"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.agentCall(callCtx, member.Session(), "capabilities", false, nil, &caps)
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Message == "unknown_operation" {
		return 0, nil
	}
	return caps.StorageAddressingProtocol, err
}

// addressingSupport remembers, for a while, Agents that advertised
// agentservice.StorageAddressingProtocol. Like rangedDownloadSupport it keeps
// no negative answer, so an Agent upgraded in place is noticed at once.
type addressingSupport struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (a *addressingSupport) supported(ctx context.Context, member cluster.Member, query func(context.Context, cluster.Member) (int, error)) (bool, error) {
	now := time.Now()
	a.mu.Lock()
	until, known := a.until[member.ID]
	a.mu.Unlock()
	if known && now.Before(until) {
		return true, nil
	}
	protocol, err := query(ctx, member)
	if err != nil || protocol < agentservice.StorageAddressingProtocol {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.until == nil {
		a.until = map[string]time.Time{}
	}
	a.until[member.ID] = now.Add(rangedSupportTTL)
	return true, nil
}
func installStorageRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/master/s3-config", s.adminOnly(s.listStorage))
	s.mux.HandleFunc("POST /api/master/s3-config", s.adminOnly(s.saveStorage))
	s.mux.HandleFunc("PUT /api/master/s3-config/{alias}", s.adminOnly(s.saveStorage))
	s.mux.HandleFunc("DELETE /api/master/s3-config/{alias}", s.adminOnly(s.deleteStorage))
	s.mux.HandleFunc("GET /api/master/s3-config/{alias}/buckets", s.adminOnly(s.listStorageBuckets))
	s.mux.HandleFunc("POST /api/master/s3-config/{alias}/permissions", s.adminOnly(s.checkStoragePermissions))
}
func (s *Server) listStorage(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	// Public configuration is deliberately a different type from the snapshot
	// sent to execution Agents. Adding a new credential to Config cannot expose
	// it through this management endpoint by accident.
	type connection struct {
		Alias                  string `json:"alias"`
		Endpoint               string `json:"endpoint"`
		Region                 string `json:"region,omitempty"`
		UseV2SignerType        *bool  `json:"useV2SignerType"`
		PathStyleEnable        *bool  `json:"pathStyleEnable"`
		PathStyleEffective     bool   `json:"pathStyleEffective"`
		CredentialsConfigured  bool   `json:"credentialsConfigured"`
		SessionTokenConfigured bool   `json:"sessionTokenConfigured"`
	}
	connections := make([]connection, 0, len(snapshot.Connections))
	for _, config := range snapshot.Connections {
		connections = append(connections, connection{
			Alias: config.Alias, Endpoint: config.Endpoint, Region: config.Region,
			UseV2SignerType: config.UseV2SignerType, PathStyleEnable: config.PathStyleEnable,
			PathStyleEffective:     config.EffectivePathStyle(),
			CredentialsConfigured:  config.AccessKey != "" && config.SecretKey != "",
			SessionTokenConfigured: config.SessionToken != "",
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"success": true, "configs": connections, "revision": snapshot.Revision})
}

// Some storage providers include an invalid access key in their error message.
// Preserve typed status checks while keeping those responses out of the UI.
type redactedStorageError struct {
	error
	message string
}

func (e redactedStorageError) Error() string { return e.message }
func (e redactedStorageError) Unwrap() error { return e.error }

func redactStorageError(err error, configs []storageconfig.Config) error {
	if err == nil {
		return nil
	}
	credentials := []string{}
	for _, config := range configs {
		for _, value := range []string{config.AccessKey, config.SecretKey, config.SessionToken} {
			if value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	sort.Slice(credentials, func(i, j int) bool { return len(credentials[i]) > len(credentials[j]) })
	pairs := make([]string, 0, len(credentials)*2)
	for _, value := range credentials {
		pairs = append(pairs, value, "[redacted]")
	}
	return redactedStorageError{error: err, message: strings.NewReplacer(pairs...).Replace(err.Error())}
}
func (s *Server) saveStorage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		storageconfig.Config
		SessionToken *string `json:"sessionToken"`
		Validate     bool    `json:"validate"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	configs := storageconfig.NewMaster(s.store)
	var revision uint64
	if r.Method == "PUT" {
		snapshot, err := configs.Snapshot()
		if err != nil {
			fail(w, 500, "Cannot read storage connections")
			return
		}
		revision = snapshot.Revision
		found := false
		for _, stored := range snapshot.Connections {
			if stored.Alias != r.PathValue("alias") {
				continue
			}
			found = true
			if (request.AccessKey == "") != (request.SecretKey == "") {
				fail(w, 400, "Provide both Access Key and Secret Key to replace credentials")
				return
			}
			if request.AccessKey == "" {
				request.Config.AccessKey, request.Config.SecretKey = stored.AccessKey, stored.SecretKey
				if request.SessionToken == nil {
					request.Config.SessionToken = stored.SessionToken
				}
			}
			break
		}
		if !found {
			storageError(w, storageconfig.ErrNotFound)
			return
		}
	}
	if request.SessionToken != nil {
		request.Config.SessionToken = *request.SessionToken
	}
	if err := storageconfig.Validate(request.Config); err != nil {
		fail(w, 400, err.Error())
		return
	}
	var warning *storageProblem
	if request.Validate {
		outcome, err := s.checkStorage(r.Context(), request.Config, "")
		if err != nil {
			fail(w, 400, localize(w, "Storage validation failed: ")+localize(w, err.Error()))
			return
		}
		if outcome.failure == nil {
			outcome.failure = outcome.saveFailure
		}
		if outcome.failure != nil {
			storageFailureResponse(w, 400, "Storage validation failed: ", outcome.failure)
			return
		}
		warning = outcome.warning
	}
	var snapshot storageconfig.Snapshot
	var err error
	if r.Method == "POST" {
		snapshot, err = configs.Create(request.Config)
	} else {
		snapshot, err = configs.UpdateAtRevision(r.PathValue("alias"), request.Config, revision)
	}
	if err != nil {
		storageError(w, err)
		return
	}
	s.storageSaved(w, r, snapshot, warning)
}
func storageError(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, storageconfig.ErrNotFound):
		status = 404
	case errors.Is(err, storageconfig.ErrExists):
		status = 409
	case errors.Is(err, storageconfig.ErrRevisionConflict):
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
	s.storageSaved(w, r, snapshot, nil)
}

// storageSaved reports the synchronization of a saved snapshot; warning is a
// non-blocking finding of the connection check.
func (s *Server) storageSaved(w http.ResponseWriter, r *http.Request, snapshot storageconfig.Snapshot, warning *storageProblem) {
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Connection saved; cannot read Agent synchronization status")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	displays := s.nodeDisplaySnapshot()
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
				details[i] = displays.lookup(member.ID, member.Address).DisplayLabel + ": " + localize(w, "等待同步")
				return
			}
			if err := s.syncStorage(ctx, member, snapshot); err != nil {
				details[i] = displays.lookup(member.ID, member.Address).DisplayLabel + ": " + localize(w, "等待同步")
			} else {
				success[i] = true
				details[i] = displays.lookup(member.ID, member.Address).DisplayLabel + ": " + localize(w, "同步成功")
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
	result := map[string]any{"success": true, "message": localize(w, "存储连接已保存"), "revision": snapshot.Revision, "successCount": count, "failedCount": len(members) - count, "partial": count < len(members), "details": details}
	storageWarningFields(w, result, warning)
	writeJSON(w, 200, result)
}
func (s *Server) listStorageBuckets(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	for _, config := range snapshot.Connections {
		if config.Alias == r.PathValue("alias") {
			// An optional bucket is checked instead of the first listed one,
			// which also checks keys limited to one bucket.
			bucket := r.URL.Query().Get("bucket")
			if bucket != "" {
				if _, err := config.S3Config(bucket); err != nil {
					fail(w, 400, err.Error())
					return
				}
			}
			outcome, err := s.checkStorage(r.Context(), config, bucket)
			if err != nil {
				fail(w, 502, err.Error())
				return
			}
			if outcome.failure != nil {
				storageFailureResponse(w, 502, "", outcome.failure)
				return
			}
			result := map[string]any{"success": true, "buckets": outcome.check.Buckets, "pathStyle": outcome.pathStyle, "pathStyleAuto": config.PathStyleEnable == nil}
			// checkedBucket names a bucket whose check passed; a bucket whose
			// check failed without blocking is named by bucketCheckFailed,
			// and the warning says why.
			if outcome.check.Bucket != "" && outcome.check.BucketError == "" {
				result["checkedBucket"] = outcome.check.Bucket
			} else if outcome.check.Bucket != "" {
				result["bucketCheckFailed"] = outcome.check.Bucket
			}
			if !outcome.complete {
				result["bucketCheckSupported"] = false
			}
			storageWarningFields(w, result, outcome.warning)
			writeJSON(w, 200, result)
			return
		}
	}
	fail(w, 404, "Storage connection not found")
}
