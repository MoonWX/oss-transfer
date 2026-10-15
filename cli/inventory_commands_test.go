package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This fixture checks the client's actual wire batching and lost-response
// replay. Durable task allocation is covered by the Master integration tests.
type inventoryWire struct {
	mu           sync.Mutex
	imports      map[string]*wireImport
	requests     int
	created      int
	drop         string
	beforeBegin  func()
	allKeys      []string
	maxBatch     int
	lastRootBody map[string]interface{}
}
type wireImport struct {
	body    []byte
	id      string
	digest  string
	batches map[int][]string
	taskID  int
}

func (f *inventoryWire) server(t *testing.T) *httptest.Server {
	t.Helper()
	f.imports = map[string]*wireImport{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		var item *wireImport
		stage := ""
		if r.URL.Path == "/api/master/inventory-imports" && r.Method == http.MethodPost {
			stage = "begin"
			body, _ := io.ReadAll(r.Body)
			var dto map[string]interface{}
			if err := json.Unmarshal(body, &dto); err != nil {
				t.Error(err)
			}
			f.lastRootBody = dto
			key := r.Header.Get("Idempotency-Key")
			if err := validateCreationRequestID(key); err != nil {
				t.Error(err)
			}
			item = f.imports[key]
			if item == nil {
				item = &wireImport{body: body, id: fmt.Sprintf("import-%d", len(f.imports)+1), digest: dto["contentSha256"].(string), batches: map[int][]string{}}
				f.imports[key] = item
			} else if !bytes.Equal(item.body, body) {
				http.Error(w, `{"success":false,"message":"changed manifest for request identity"}`, 409)
				return
			}
			if f.beforeBegin != nil {
				f.beforeBegin()
				f.beforeBegin = nil
			}
		} else {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/master/inventory-imports/"), "/")
			for _, candidate := range f.imports {
				if candidate.id == parts[0] {
					item = candidate
				}
			}
			if item == nil {
				http.NotFound(w, r)
				return
			}
			if len(parts) == 3 && parts[1] == "batches" && r.Method == http.MethodPut {
				stage = "batch"
				var number int
				fmt.Sscan(parts[2], &number)
				var dto struct {
					Keys []string `json:"keys"`
				}
				json.NewDecoder(r.Body).Decode(&dto)
				if len(dto.Keys) < 1 || len(dto.Keys) > 128 {
					t.Errorf("invalid batch: %d", len(dto.Keys))
				}
				if old, exists := item.batches[number]; exists && !reflect.DeepEqual(old, dto.Keys) {
					http.Error(w, `{"success":false}`, 409)
					return
				}
				item.batches[number] = dto.Keys
				if len(dto.Keys) > f.maxBatch {
					f.maxBatch = len(dto.Keys)
				}
			} else if len(parts) == 2 && parts[1] == "finalize" && r.Method == http.MethodPost {
				stage = "finalize"
				var dto struct {
					BatchCount int `json:"batchCount"`
				}
				json.NewDecoder(r.Body).Decode(&dto)
				hash := sha256.New()
				keys := []string{}
				for number := 1; number <= dto.BatchCount; number++ {
					batch, found := item.batches[number]
					if !found {
						t.Errorf("batch gap at %d", number)
						http.Error(w, `{"success":false}`, 409)
						return
					}
					for _, key := range batch {
						io.WriteString(hash, key+"\n")
						keys = append(keys, key)
					}
				}
				if hex.EncodeToString(hash.Sum(nil)) != item.digest {
					t.Error("uploaded contents differ from immutable snapshot digest")
				}
				if item.taskID == 0 {
					f.created++
					item.taskID = f.created
					f.allKeys = append(f.allKeys, keys...)
				}
			} else {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
		}
		if stage == f.drop {
			f.drop = ""
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		state := "UPLOADING"
		if item.taskID > 0 {
			state = "FINALIZED"
		}
		unique := map[string]bool{}
		for _, batch := range item.batches {
			for _, key := range batch {
				unique[key] = true
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"id": item.id, "state": state, "taskId": item.taskID, "uniqueCount": len(unique)}})
	}))
	t.Cleanup(server.Close)
	return server
}

func inventoryDirectory(t *testing.T, contents map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	for name, content := range contents {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestInventoryClientBatchesImmutableSnapshotAndKeepsRawKeys(t *testing.T) {
	keys := []string{" leading/trailing ", "中文/照片.jpg", "duplicate", "duplicate"}
	for i := 0; i < 257; i++ {
		keys = append(keys, fmt.Sprintf("object/%03d", i))
	}
	directory := inventoryDirectory(t, map[string]string{"objects.incr": "\r\n  \t\r\n" + strings.Join(keys, "\r\n") + "\r\n", "ignored.txt": "ignored"})
	fixture := &inventoryWire{beforeBegin: func() { os.WriteFile(filepath.Join(directory, "objects.incr"), []byte("mutated-on-disk\n"), 0600) }}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	result, err := runInventoryImport(client, []string{"source/bucket", "target/copy", directory, "--node", "node-b", "--node", "node-a", "--overwrite"})
	if err != nil || result["success"] != true || result["taskCount"] != 1 {
		t.Fatalf("result: %#v, %v", result, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.created != 1 || fixture.maxBatch != 128 || !reflect.DeepEqual(fixture.allKeys, keys) {
		t.Fatalf("uploaded snapshot mismatch: tasks=%d batch=%d keys=%v", fixture.created, fixture.maxBatch, fixture.allKeys)
	}
	if fixture.lastRootBody["overwrite"] != true || fixture.lastRootBody["name"] != "objects.incr" || fixture.lastRootBody["inventoryDir"] != nil {
		t.Fatalf("body: %#v", fixture.lastRootBody)
	}
	if !reflect.DeepEqual(fixture.lastRootBody["eligibleWorkerNodes"], []interface{}{"node-a", "node-b"}) {
		t.Fatal(fixture.lastRootBody)
	}
}

func TestInventoryClientLostResponseReplayDoesNotCreateAnotherTask(t *testing.T) {
	for _, stage := range []string{"begin", "batch", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			directory := inventoryDirectory(t, map[string]string{"objects.incr": "one\ntwo\n"})
			fixture := &inventoryWire{drop: stage}
			server := fixture.server(t)
			client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
			args := []string{"source/bucket", "target/copy", directory}
			result, err := runInventoryImport(client, args)
			id, _ := result["requestId"].(string)
			if err == nil || id == "" || !strings.Contains(err.Error(), "--request-id "+id) {
				t.Fatalf("lost response lost identity: %#v %v", result, err)
			}
			fixture.mu.Lock()
			requests := fixture.requests
			fixture.mu.Unlock()
			want := map[string]int{"begin": 1, "batch": 2, "finalize": 3}[stage]
			if requests != want {
				t.Fatalf("mutation was automatically replayed: %d want %d", requests, want)
			}
			result, err = runInventoryImport(client, append(args, "--request-id", id))
			if err != nil || result["success"] != true {
				t.Fatalf("replay: %#v %v", result, err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.created != 1 || len(fixture.imports) != 1 {
				t.Fatalf("duplicate task after replay: %d imports:%d", fixture.created, len(fixture.imports))
			}
		})
	}
}

func TestInventoryClientRejectsAllBadInputBeforeNetwork(t *testing.T) {
	for _, bad := range []string{strings.Repeat("x", 1025), strings.Repeat("x", 10000), "bad\x00key", "embedded\rcarriage", string([]byte{0xff})} {
		t.Run(fmt.Sprintf("size-%d", len(bad)), func(t *testing.T) {
			dir := inventoryDirectory(t, map[string]string{"a-valid.incr": "valid\n", "z-bad.incr": bad + "\n"})
			fixture := &inventoryWire{}
			server := fixture.server(t)
			client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
			_, err := runInventoryImport(client, []string{"source/bucket", "target/copy", dir})
			if err == nil {
				t.Fatal("invalid key accepted")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.requests != 0 {
				t.Fatalf("invalid second file left partial submission: %d", fixture.requests)
			}
		})
	}
}

func TestInventoryClientChangedFileRejectsReuseOfRequestIdentity(t *testing.T) {
	dir := inventoryDirectory(t, map[string]string{"objects.incr": "original\n"})
	fixture := &inventoryWire{}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	args := []string{"source/bucket", "target/copy", dir, "--request-id", "fixed"}
	if _, err := runInventoryImport(client, args); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "objects.incr"), []byte("changed\n"), 0600)
	result, err := runInventoryImport(client, args)
	if err == nil || !strings.Contains(err.Error(), "409") || result["requestId"] != "fixed" {
		t.Fatalf("changed replay: %#v %v", result, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.created != 1 || len(fixture.imports) != 1 {
		t.Fatal("changed manifest silently created a new task")
	}
}

func TestInventoryJSONOutputAndEmptyFile(t *testing.T) {
	dir := inventoryDirectory(t, map[string]string{"empty.incr": "\n\r\n  \n"})
	fixture := &inventoryWire{}
	server := fixture.server(t)
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "fixture-token")
	var out, errors bytes.Buffer
	code := runCLI([]string{"--host", server.URL, "--json", "import-inventory", "source/bucket", "target/copy", dir}, strings.NewReader(""), &out, &errors)
	var result map[string]interface{}
	if code != 0 || json.Unmarshal(out.Bytes(), &result) != nil || result["success"] != true {
		t.Fatalf("code=%d output=%s errors=%s", code, &out, &errors)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.created != 1 || fixture.maxBatch != 0 || len(fixture.allKeys) != 0 {
		t.Fatal("empty inventory read bucket contents or did not finalize")
	}
}
