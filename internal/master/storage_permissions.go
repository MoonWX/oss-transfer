package master

import (
	"errors"
	"net/http"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// checkStoragePermissions runs, on one online Agent, the requests a migration
// makes on one bucket and reports each one: listing objects, reading an
// object's metadata and content and, when write is requested, writing,
// reading back, a multipart upload and its abort, and removing a small probe
// object, below an optional write prefix.
func (s *Server) checkStoragePermissions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bucket string `json:"bucket"`
		Write  bool   `json:"write"`
		// Prefix is where the write check writes, for keys that may only
		// write below a prefix. It is ignored without write.
		Prefix string `json:"prefix,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	var config *storageconfig.Config
	for i := range snapshot.Connections {
		if snapshot.Connections[i].Alias == r.PathValue("alias") {
			config = &snapshot.Connections[i]
		}
	}
	if config == nil {
		fail(w, 404, "Storage connection not found")
		return
	}
	if body.Bucket == "" {
		fail(w, 400, "Enter the bucket to check")
		return
	}
	if _, err := config.S3Config(body.Bucket); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if !transferstore.ValidPermissionPrefix(body.Prefix) {
		fail(w, 400, "The write prefix must be at most 512 bytes of text without control characters")
		return
	}
	if !body.Write {
		body.Prefix = ""
	}
	members, err := s.onlineAgents()
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if len(members) == 0 {
		fail(w, 409, "no Agent is online; join an Agent before validating storage")
		return
	}
	request := struct {
		Config storageconfig.Config `json:"config"`
		Bucket string               `json:"bucket"`
		Write  bool                 `json:"write,omitempty"`
		Prefix string               `json:"prefix,omitempty"`
	}{*config, body.Bucket, body.Write, body.Prefix}
	var check transferstore.PermissionCheck
	err = s.agentCall(r.Context(), members[0].Session(), "storage_permissions", false, request, &check)
	var agentError *AgentError
	if errors.As(err, &agentError) && agentError.Status == http.StatusNotFound && agentError.Message == "unknown_operation" {
		fail(w, 409, "The execution node is too old to check bucket permissions; update it first")
		return
	}
	// Agents before the write prefix refuse the unknown field before they
	// run anything.
	if errors.As(err, &agentError) && body.Prefix != "" && agentError.Status == http.StatusBadRequest && strings.Contains(agentError.Message, `unknown field "prefix"`) {
		fail(w, 409, "The execution node is too old to check writing below a prefix; update it first, or check without a write prefix")
		return
	}
	if err != nil {
		fail(w, 502, redactStorageError(err, []storageconfig.Config{*config}).Error())
		return
	}
	results := make([]map[string]any, 0, len(check.Results))
	for _, item := range check.Results {
		row := map[string]any{"operation": item.Operation, "status": item.Status}
		if item.Key != "" {
			row["key"] = item.Key
		}
		if item.Reason != "" {
			row["reason"] = item.Reason
		}
		if item.Status == transferstore.PermissionFailed {
			detail := redactStorageError(errors.New(item.Error), []storageconfig.Config{*config}).Error()
			problem := newStorageProblem(item.ErrorKind, detail, *config, check.Bucket, check.PathStyle)
			row["message"], row["detail"], row["reasonCode"] = problem.summary(w), detail, problem.reasonCode()
		}
		results = append(results, row)
	}
	display := s.nodeDisplaySnapshot().lookup(members[0].ID, members[0].Address)
	result := map[string]any{"success": true, "bucket": check.Bucket, "pathStyle": check.PathStyle, "write": check.Write, "results": results, "node": display.DisplayLabel}
	if check.Prefix != "" {
		result["prefix"] = check.Prefix
	}
	if check.LeftoverKey != "" {
		result["leftoverKey"] = check.LeftoverKey
	}
	if check.LeftoverUploadKey != "" {
		result["leftoverUploadKey"], result["leftoverUploadId"] = check.LeftoverUploadKey, check.LeftoverUploadID
	}
	writeJSON(w, 200, result)
}
