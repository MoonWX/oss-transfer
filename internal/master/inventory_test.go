package master

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

func manifestSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func inventoryCall(t *testing.T, f *agentHTTPFixture, method, path, key, token string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r, err := http.NewRequest(method, f.http.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, err := f.http.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	if err = json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, out
}
func TestInventoryHTTPSealAtomicTaskBindingReplayAndAgentReplication(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	config := storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "fixture-ak", SecretKey: "fixture-sk"}
	if _, err := storageconfig.NewMaster(f.db).Create(config); err != nil {
		t.Fatal(err)
	}
	begin := inventoryRequest{SourceS3Alias: "qa", SourceBucket: "source", DestS3Alias: "qa", DestBucket: "destination", EligibleWorkerNodes: []string{node.Address}, Name: "keys.incr", ContentSHA256: manifestSHA(" spaced \na\n spaced \n")}
	code, out := inventoryCall(t, f, "POST", "/api/master/inventory-imports", "logical-import", f.admin, begin)
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	base := "/api/master/inventory-imports/" + id
	if p, err := f.master.tasks.repo.List(0, 10, false); err != nil || len(p) != 0 {
		t.Fatal("upload created task", p, err)
	}
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, map[string]any{"batchCount": 0, "uniqueCount": 0}); code != 409 {
		t.Fatal("partial content accepted", code, out)
	}
	body := map[string]any{"keys": []string{" spaced ", "a", " spaced "}}
	if code, out = inventoryCall(t, f, "PUT", base+"/batches/1", "", f.admin, body); code != 200 || out["data"].(map[string]any)["uniqueCount"] != float64(2) {
		t.Fatal(code, out)
	}
	config.SecretKey = "rotated-fixture-key"
	if _, err := storageconfig.NewMaster(f.db).Update("qa", config); err != nil {
		t.Fatal(err)
	}
	seal := map[string]any{"batchCount": 1, "uniqueCount": 2}
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, seal); code != 409 {
		t.Fatal("connection changed", code, out)
	}
	if parents, _ := f.master.tasks.repo.List(0, 10, false); len(parents) != 0 {
		t.Fatal("failed guard created task")
	}
	config.SecretKey = "fixture-sk"
	_, _ = storageconfig.NewMaster(f.db).Update("qa", config)
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, seal); code != 200 {
		t.Fatal(code, out)
	}
	taskID := int64(out["data"].(map[string]any)["taskId"].(float64))
	p, err := f.master.tasks.repo.Get(taskID)
	if err != nil || p.Spec.InventoryID != id {
		t.Fatal(p, err)
	}
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, seal); code != 200 || out["data"].(map[string]any)["taskId"] != float64(taskID) {
		t.Fatal("finalize not idempotent", code, out)
	}
	if code, out = inventoryCall(t, f, "POST", "/api/master/inventory-imports", "logical-import", f.admin, begin); code != 200 || out["data"].(map[string]any)["state"] != "FINALIZED" {
		t.Fatal(code, out)
	}
	if code, out = inventoryCall(t, f, "PUT", base+"/batches/1", "", f.admin, body); code != 200 {
		t.Fatal("lost upload reply cannot replay", code, out)
	}
	if code, out = inventoryCall(t, f, "DELETE", base, "", f.admin, nil); code != 409 {
		t.Fatal("cancelled live task manifest", code, out)
	}
	for range 2 {
		if ready, err := f.master.tasks.syncInventory(context.Background(), p); err != nil || !ready {
			t.Fatal(ready, err)
		}
	}
	h, err := service.Inventory.Get(context.Background(), id)
	if err != nil || !inventory.Ready(h) || h.ParentID != taskID || h.UniqueCount != 2 {
		t.Fatal(h, err)
	}
	keys, _, err := service.Inventory.Keys(context.Background(), id, "", 128)
	if err != nil || len(keys) != 2 {
		t.Fatal(keys, err)
	}
	row := taskDTO(p)
	if row["inventoryManifestId"] != id {
		t.Fatal("export lost inventory scope")
	}
	raw, _ := json.Marshal([]any{row})
	rec := httptest.NewRecorder()
	if _, ok := readTaskRows(rec, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(raw))); ok {
		t.Fatal("inventory export became bucket scan")
	}
	parents, _ := f.master.tasks.repo.List(0, 10, false)
	if len(parents) != 1 {
		t.Fatal("duplicate task", parents)
	}
}
func TestInventoryHTTPAuthCancelAndRevokedEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	_, err := storageconfig.NewMaster(f.db).Create(storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "fixture-ak", SecretKey: "fixture-sk"})
	if err != nil {
		t.Fatal(err)
	}
	begin := inventoryRequest{SourceS3Alias: "qa", SourceBucket: "source", DestS3Alias: "qa", DestBucket: "destination", Name: "empty.incr", ContentSHA256: manifestSHA("")}
	code, out := inventoryCall(t, f, "POST", "/api/master/inventory-imports", "key", "", begin)
	if code != 401 {
		t.Fatal(code, out)
	}
	code, out = inventoryCall(t, f, "POST", "/api/master/inventory-imports", "key", f.admin, begin)
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	base := "/api/master/inventory-imports/" + id
	// The same authenticated route must not expose another administrator's upload.
	if err = f.db.Update(func(tx *state.Tx) error {
		var login session
		if _, e := tx.Get("sessions", tokenKey(f.admin), &login); e != nil {
			return e
		}
		var user User
		if _, e := tx.Get("users", login.Username, &user); e != nil {
			return e
		}
		user.Username = "second-admin"
		login.Username = user.Username
		if e := tx.Put("users", user.Username, user); e != nil {
			return e
		}
		return tx.Put("sessions", tokenKey("second-token"), login)
	}); err != nil {
		t.Fatal(err)
	}
	if code, out = inventoryCall(t, f, "GET", base, "", "second-token", nil); code != 403 {
		t.Fatal(code, out)
	}
	if _, err = f.master.agents.registry.Revoke(node.ID); err != nil {
		t.Fatal(err)
	}
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, map[string]any{"batchCount": 0, "uniqueCount": 0}); code == 200 {
		t.Fatal("revoked enrollment finalized", out)
	}
	if parents, _ := f.master.tasks.repo.List(0, 10, false); len(parents) != 0 {
		t.Fatal("revoked node created task")
	}
	for range 2 {
		if code, out = inventoryCall(t, f, "DELETE", base, "", f.admin, nil); code != 200 || out["data"].(map[string]any)["state"] != "CANCELLED" {
			t.Fatal(code, out)
		}
	}
	if code, out = inventoryCall(t, f, "POST", base+"/finalize", "", f.admin, map[string]any{"batchCount": 0, "uniqueCount": 0}); code != 409 {
		t.Fatal("cancelled upload revived", code, out)
	}
	changed := begin
	changed.ContentSHA256 = manifestSHA("new\n")
	if code, out = inventoryCall(t, f, "POST", "/api/master/inventory-imports", "key", f.admin, changed); code != 409 {
		t.Fatal("changed source reused request id", code, out)
	}
}
func TestInventoryFinalizeTransactionRechecksAuthorization(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	_, _ = storageconfig.NewMaster(f.db).Create(storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "a", SecretKey: "b"})
	begin := inventoryRequest{SourceS3Alias: "qa", SourceBucket: "src", DestS3Alias: "qa", DestBucket: "dst", EligibleWorkerNodes: []string{node.ID}, Name: "empty.incr", ContentSHA256: manifestSHA("")}
	code, out := inventoryCall(t, f, "POST", "/api/master/inventory-imports", "atomic", f.admin, begin)
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	binding, err := f.master.inventoryBinding(id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = inventory.New(f.db).Seal(context.Background(), inventory.Seal{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	guard := f.master.inventoryFinalizeCheck(r, binding)
	_ = f.db.Update(func(tx *state.Tx) error {
		var user User
		_, e := tx.Get("users", "admin", &user)
		if e != nil {
			return e
		}
		user.Revision++
		return tx.Put("users", "admin", user)
	})
	if _, _, err = f.master.tasks.repo.CreateChecked(binding.Spec, "inventory:"+id, guard); err == nil {
		t.Fatal("stale permission passed commit guard")
	}
	h, err := inventory.New(f.db).Get(context.Background(), id)
	if err != nil || h.TaskID != 0 || h.State != "SEALED" {
		t.Fatal("failed transaction bound manifest", h, err)
	}
	if p, _ := f.master.tasks.repo.List(0, 10, false); len(p) != 0 {
		t.Fatal(fmt.Sprintf("failed transaction leaked %d task(s)", len(p)))
	}
}
