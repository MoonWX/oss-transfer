package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type inventoryRequest struct {
	SourceS3Alias       string   `json:"sourceS3Alias"`
	SourceBucket        string   `json:"sourceBucket"`
	DestS3Alias         string   `json:"destS3Alias"`
	DestBucket          string   `json:"destBucket"`
	EligibleWorkerNodes []string `json:"eligibleWorkerNodes,omitempty"`
	Overwrite           bool     `json:"overwrite"`
	Name                string   `json:"name"`
	ContentSHA256       string   `json:"contentSha256"`
}
type inventoryBinding struct {
	Owner              string    `json:"owner"`
	RequestFingerprint string    `json:"requestFingerprint"`
	SnapshotHash       string    `json:"snapshotHash"`
	Spec               task.Spec `json:"spec"`
}

func inventoryDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func installInventoryRoutes(s *Server) {
	s.mux.HandleFunc("DELETE /api/master/inventory-imports/{id}", s.adminOnly(s.inventoryHTTP))
	s.mux.HandleFunc("POST /api/master/inventory-imports", s.adminOnly(s.beginInventoryHTTP))
	s.mux.HandleFunc("GET /api/master/inventory-imports/{id}", s.adminOnly(s.inventoryHTTP))
	s.mux.HandleFunc("PUT /api/master/inventory-imports/{id}/batches/{number}", s.adminOnly(s.inventoryHTTP))
	s.mux.HandleFunc("POST /api/master/inventory-imports/{id}/finalize", s.adminOnly(s.inventoryHTTP))
}
func inventoryError(w http.ResponseWriter, err error) {
	status := 400
	switch {
	case errors.Is(err, inventory.ErrNotFound):
		status = 404
	case errors.Is(err, inventory.ErrConflict), errors.Is(err, inventory.ErrUnsealed), errors.Is(err, task.ErrConflict):
		status = 409
	case errors.Is(err, inventory.ErrCapacity):
		status = 429
	}
	fail(w, status, err.Error())
}
func inventoryResult(w http.ResponseWriter, h inventory.Header) {
	out := map[string]any{"success": true, "data": h}
	if h.TaskID > 0 {
		out["taskIds"] = []int64{h.TaskID}
	}
	writeJSON(w, 200, out)
}
func (s *Server) inventoryBinding(id string) (b inventoryBinding, err error) {
	err = s.store.View(func(tx *state.Tx) error {
		found, e := tx.Get("master-inventory", id, &b)
		if e == nil && !found {
			return inventory.ErrNotFound
		}
		return e
	})
	return
}
func inventorySnapshot(snapshot storageconfig.Snapshot, spec task.Spec) (string, error) {
	configs := make([]storageconfig.Config, 0, 2)
	for _, endpoint := range []struct{ alias, bucket string }{{spec.Source.Ref, spec.Source.Bucket}, {spec.Destination.Ref, spec.Destination.Bucket}} {
		found := false
		for _, config := range snapshot.Connections {
			if config.Alias == endpoint.alias {
				if _, err := config.S3Config(endpoint.bucket); err != nil {
					return "", err
				}
				configs = append(configs, config)
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("storage connection %s does not exist", endpoint.alias)
		}
	}
	return inventoryDigest(configs), nil
}
func (s *Server) beginInventoryHTTP(w http.ResponseWriter, r *http.Request) {
	var request inventoryRequest
	if !readJSON(w, r, &request) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 220 || strings.ContainsAny(key, "\r\n\x00") {
		fail(w, 400, "Idempotency-Key is required (maximum 220 bytes)")
		return
	}
	user, valid, err := s.authenticated(r)
	if err != nil || !valid {
		fail(w, 401, "Sign in required")
		return
	}
	id := inventoryDigest([]string{user.Username, key})
	fingerprint := inventoryDigest(request)
	old, err := s.inventoryBinding(id)
	if err == nil {
		if old.Owner != user.Username || old.RequestFingerprint != fingerprint {
			inventoryError(w, inventory.ErrConflict)
			return
		}
		h, e := inventory.New(s.store).Get(r.Context(), id)
		if e != nil {
			inventoryError(w, e)
			return
		}
		inventoryResult(w, h)
		return
	}
	if !errors.Is(err, inventory.ErrNotFound) {
		inventoryError(w, err)
		return
	}
	spec, _, err := s.resolveTaskSpec(r.Context(), taskCreateRequest{SourceS3Alias: request.SourceS3Alias, SourceBucket: request.SourceBucket, DestS3Alias: request.DestS3Alias, DestBucket: request.DestBucket, EligibleWorkerNodes: request.EligibleWorkerNodes, Overwrite: &request.Overwrite, DistributionMode: "BALANCED"}, "add-buckets")
	if err != nil {
		inventoryError(w, err)
		return
	}
	spec.InventoryID = id
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		inventoryError(w, err)
		return
	}
	snapshotHash, err := inventorySnapshot(snapshot, spec)
	if err != nil {
		inventoryError(w, err)
		return
	}
	binding := inventoryBinding{Owner: user.Username, RequestFingerprint: fingerprint, SnapshotHash: snapshotHash, Spec: spec}
	descriptor := inventory.Descriptor{ID: id, Name: request.Name, ContentSHA256: request.ContentSHA256, Binding: inventoryDigest(binding), SpecSHA256: inventoryDigest(spec)}
	var h inventory.Header
	err = s.store.Update(func(tx *state.Tx) error {
		var prior inventoryBinding
		found, e := tx.Get("master-inventory", id, &prior)
		if e != nil {
			return e
		}
		if found {
			if prior.Owner != user.Username || prior.RequestFingerprint != fingerprint {
				return inventory.ErrConflict
			}
			return nil
		}
		h, e = inventory.BeginTx(tx, descriptor)
		if e != nil {
			return e
		}
		return tx.Put("master-inventory", id, binding)
	})
	if err == nil {
		h, err = inventory.New(s.store).Get(r.Context(), id)
	}
	if err != nil {
		inventoryError(w, err)
		return
	}
	inventoryResult(w, h)
}
func (s *Server) inventoryHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	binding, err := s.inventoryBinding(id)
	if err != nil {
		inventoryError(w, err)
		return
	}
	user, valid, err := s.authenticated(r)
	if err != nil || !valid {
		fail(w, 401, "Sign in required")
		return
	}
	if user.Username != binding.Owner {
		fail(w, 403, "Inventory belongs to another user")
		return
	}
	repo := inventory.New(s.store)
	if r.Method == http.MethodDelete {
		h, e := repo.Cancel(r.Context(), id)
		if e != nil {
			inventoryError(w, e)
			return
		}
		inventoryResult(w, h)
		return
	}
	if r.Method == http.MethodGet {
		h, e := repo.Get(r.Context(), id)
		if e != nil {
			inventoryError(w, e)
			return
		}
		inventoryResult(w, h)
		return
	}
	if r.Method == http.MethodPut {
		number, e := strconv.Atoi(r.PathValue("number"))
		if e != nil {
			inventoryError(w, inventory.ErrInvalid)
			return
		}
		var body struct {
			Keys []string `json:"keys"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&body); e != nil {
			fail(w, 400, "Invalid inventory batch JSON (maximum 1 MiB)")
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			fail(w, 400, "Expected one inventory batch")
			return
		}
		h, e := repo.Put(r.Context(), inventory.Batch{ID: id, Number: number, Keys: body.Keys})
		if e != nil {
			inventoryError(w, e)
			return
		}
		inventoryResult(w, h)
		return
	}
	var body struct {
		BatchCount  int    `json:"batchCount"`
		UniqueCount *int64 `json:"uniqueCount"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.UniqueCount == nil {
		fail(w, 400, "uniqueCount from the last batch response is required")
		return
	}
	h, err := repo.Seal(r.Context(), inventory.Seal{ID: id, BatchCount: body.BatchCount, UniqueCount: *body.UniqueCount})
	if err != nil {
		inventoryError(w, err)
		return
	}
	if h.TaskID > 0 {
		inventoryResult(w, h)
		return
	}
	_, err = s.createTransfer(r.Context(), binding.Spec, "inventory:"+id, s.inventoryFinalizeCheck(r, binding))
	if err != nil {
		inventoryError(w, err)
		return
	}
	h, err = repo.Get(r.Context(), id)
	if err != nil {
		inventoryError(w, err)
		return
	}
	inventoryResult(w, h)
}

func (t *taskRuntime) syncInventory(ctx context.Context, p task.Parent) (bool, error) {
	if p.Spec.InventoryID == "" {
		return true, nil
	}
	repo := inventory.New(t.server.store)
	h, err := repo.Get(ctx, p.Spec.InventoryID)
	if err != nil {
		return false, err
	}
	if !inventory.Ready(h) || h.TaskID != p.ID {
		return false, inventory.ErrUnsealed
	}
	descriptor := h.Descriptor
	descriptor.ParentID = p.ID
	scanner := nodeSession(p.Spec.Nodes[0])
	var remote inventory.Header
	if err = t.server.agentCall(ctx, scanner, "inventory_begin", true, descriptor, &remote); err != nil {
		return false, err
	}
	if inventory.Ready(remote) {
		if remote.UniqueCount != h.UniqueCount || remote.NextBatch != h.NextBatch {
			return false, inventory.ErrConflict
		}
		return true, nil
	}
	// Bound each control-plane turn. Durable NextBatch resumes after disconnects.
	through := min(remote.NextBatch+16, h.NextBatch)
	for number := remote.NextBatch; number < through; number++ {
		b, err := repo.Batch(ctx, h.ID, number)
		if err != nil {
			return false, err
		}
		if err = t.server.agentCall(ctx, scanner, "inventory_batch", true, b, &remote); err != nil {
			return false, err
		}
	}
	if remote.NextBatch < h.NextBatch {
		return false, nil
	}
	err = t.server.agentCall(ctx, scanner, "inventory_seal", true, inventory.Seal{ID: h.ID, BatchCount: h.NextBatch - 1, UniqueCount: h.UniqueCount}, &remote)
	return err == nil, err
}

func (s *Server) inventoryFinalizeCheck(r *http.Request, binding inventoryBinding) func(*state.Tx) error {
	return func(tx *state.Tx) error {
		// Revalidate the same session and registered enrollment at the commit fence.
		var login session
		found, err := tx.Get("sessions", tokenKey(bearer(r)), &login)
		if err != nil {
			return err
		}
		if !found || login.Username != binding.Owner || !s.options.Now().Before(login.ExpiresAt) {
			return fmt.Errorf("%w: session expired", inventory.ErrConflict)
		}
		var user User
		found, err = tx.Get("users", binding.Owner, &user)
		if err != nil {
			return err
		}
		if !found || user.Revision != login.UserRevision || user.Role != "admin" {
			return fmt.Errorf("%w: authorization changed", inventory.ErrConflict)
		}
		for _, node := range binding.Spec.Nodes {
			var member cluster.Member
			found, err = tx.Get("agent-members", node.ID, &member)
			if err != nil {
				return err
			}
			if !found || member.Revoked || member.EnrollmentID != node.EnrollmentID || member.Address != node.Address {
				return fmt.Errorf("%w: Agent enrollment changed", inventory.ErrConflict)
			}
		}
		snapshot, err := storageconfig.MasterSnapshotTx(tx)
		if err != nil {
			return err
		}
		digest, err := inventorySnapshot(snapshot, binding.Spec)
		if err != nil {
			return err
		}
		if digest != binding.SnapshotHash {
			return fmt.Errorf("%w: storage connections changed since upload began; begin a new import", inventory.ErrConflict)
		}
		return nil
	}
}
