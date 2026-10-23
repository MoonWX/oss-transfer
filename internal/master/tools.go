package master

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func installToolRoutes(s *Server) {
	s.mux.HandleFunc("GET /api/worker/node-info", s.adminOnly(func(w http.ResponseWriter, r *http.Request) {
		config, err := s.configuration()
		if err != nil {
			fail(w, 500, "Cannot read Master configuration")
			return
		}
		writeJSON(w, 200, map[string]any{"isMaster": true, "role": "master", "masterWebUrl": config.PublicURL, "localTransferSupported": false})
	}))
	s.mux.HandleFunc("GET /api/master/object-metadata", s.adminOnly(s.compareMetadata))
	s.mux.HandleFunc("POST /api/master/comparisons", s.adminOnly(s.startComparison))
	s.mux.HandleFunc("GET /api/master/comparisons/{id}", s.adminOnly(s.getComparison))
	s.mux.HandleFunc("POST /api/master/comparisons/{id}/cancel", s.adminOnly(s.cancelComparison))
	s.mux.HandleFunc("DELETE /api/master/comparisons/{id}", s.adminOnly(s.deleteComparison))
}
func comparisonEndpoint(value string, snapshot storageconfig.Snapshot) (execution.Endpoint, error) {
	alias, bucket, hasAlias := strings.Cut(value, "/")
	if !hasAlias {
		bucket = value
		alias = ""
		for _, config := range snapshot.Connections {
			if config.Alias == "default" {
				alias = config.Alias
				break
			}
		}
		if alias == "" && len(snapshot.Connections) == 1 {
			alias = snapshot.Connections[0].Alias
		}
	}
	if alias == "" {
		return execution.Endpoint{}, errors.New("Specify a storage connection and bucket as alias/bucket")
	}
	for _, config := range snapshot.Connections {
		if config.Alias == alias {
			return ossEndpoint(alias, bucket)
		}
	}
	return execution.Endpoint{}, errors.New("Storage connection not found")
}
func metadataValue(object transferstore.Object, err error) map[string]any {
	if err != nil {
		if agentStatus(err, 404) {
			return map[string]any{"exists": false, "queryStatus": "NOT_FOUND", "statusCode": 404}
		}
		return map[string]any{"exists": false, "queryStatus": "UNAVAILABLE", "error": err.Error()}
	}
	result := map[string]any{"exists": true, "queryStatus": "FOUND", "statusCode": 200, "contentLength": object.Size, "contentType": object.ContentType, "etag": object.ETag, "userMetadata": object.Metadata, "contentEncoding": object.ContentEncoding, "contentDisposition": object.ContentDisposition, "cacheControl": object.CacheControl, "storageClass": object.StorageClass}
	if !object.ModifiedAt.IsZero() {
		result["lastModified"] = object.ModifiedAt.UnixMilli()
	}
	if !object.ExpiresAt.IsZero() {
		result["httpExpiresDate"] = object.ExpiresAt.UnixMilli()
	}
	if !object.LifecycleExpiresAt.IsZero() {
		result["expirationTime"] = object.LifecycleExpiresAt.UnixMilli()
	}
	return result
}
func (s *Server) compareMetadata(w http.ResponseWriter, r *http.Request) {
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	q := r.URL.Query()
	names := []string{"source", "dest"}
	requests := make([]agentservice.StatRequest, 2)
	for i, name := range names {
		endpoint, err := comparisonEndpoint(q.Get(name+"Bucket"), snapshot)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		key := q.Get(name + "Key")
		if key == "" {
			fail(w, 400, "Object key is required")
			return
		}
		requests[i] = agentservice.StatRequest{Endpoint: endpoint, Key: key}
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err = s.syncStorage(ctx, nodes[0], snapshot); err != nil {
		fail(w, 503, err.Error())
		return
	}
	var objects [2]transferstore.Object
	var results [2]error
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = redactStorageError(s.agentCall(ctx, nodes[0].Session(), "object_stat", false, requests[i], &objects[i]), snapshot.Connections)
		}()
	}
	wg.Wait()
	comparable := results[0] == nil && results[1] == nil
	var sizeMatch any
	if comparable {
		sizeMatch = objects[0].Size == objects[1].Size
	}
	partial := false
	for _, err := range results {
		partial = partial || (err != nil && !agentStatus(err, 404))
	}
	status := 200
	if partial {
		status = 502
	}
	writeJSON(w, status, map[string]any{"success": !partial, "partial": partial, "sourceBucket": q.Get("sourceBucket"), "destBucket": q.Get("destBucket"), "sourceKey": q.Get("sourceKey"), "destKey": q.Get("destKey"), "sourceMetadata": metadataValue(objects[0], results[0]), "destMetadata": metadataValue(objects[1], results[1]), "verification": map[string]any{"sizeMatch": sizeMatch, "message": localize(w, "仅比较对象元数据。")}})
}
