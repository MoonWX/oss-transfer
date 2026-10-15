package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// This protocol fixture actually accepts the mutation, then drops its TCP
// connection without sending response headers. It checks CLI wire behavior;
// the Master repository's durable deduplication is tested in the server module.
type acceptedCreation struct {
	mu       sync.Mutex
	requests int
	created  int
	keys     map[string][32]byte
	ids      map[string]int
	dropNext bool
	legacy   bool
}

func (f *acceptedCreation) server(t *testing.T) *httptest.Server {
	t.Helper()
	f.keys, f.ids = map[string][32]byte{}, map[string]int{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/local-nodes" {
			if r.Header.Get("Idempotency-Key") != "" {
				t.Error("read-only lookup consumed a creation identity")
			}
			io.WriteString(w, creationNodesExchange().response)
			return
		}
		if r.URL.Path == "/api/master/local-transfers/validate" {
			if r.Header.Get("Idempotency-Key") != "" {
				t.Error("validation consumed a creation identity")
			}
			io.WriteString(w, `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"}]}}`)
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/api/master/add-buckets" && r.URL.Path != "/api/master/import" && r.URL.Path != "/api/master/local-transfers") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if err := validateCreationRequestID(key); err != nil {
			t.Errorf("missing/invalid wire request identity: %v", err)
		}
		fingerprint := sha256.Sum256(append([]byte(r.URL.Path+"\n"), body...))
		f.mu.Lock()
		f.requests++
		previous, exists := f.keys[key]
		if exists && previous != fingerprint && !f.legacy {
			f.mu.Unlock()
			http.Error(w, `{"success":false,"message":"request identity conflicts with existing payload"}`, 409)
			return
		}
		if !exists || f.legacy {
			f.created++
			f.keys[key], f.ids[key] = fingerprint, f.created
		}
		id, drop := f.ids[key], f.dropNext
		f.dropNext = false
		f.mu.Unlock()
		if drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		switch r.URL.Path {
		case "/api/master/local-transfers":
			fmt.Fprintf(w, `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[%d]}}`, id)
		case "/api/master/import":
			fmt.Fprintf(w, `{"success":true,"importedCount":1,"taskIds":[%d]}`, id)
		default:
			fmt.Fprintf(w, `{"success":true,"addedCount":1,"taskIds":[%d]}`, id)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCreationRequestIdentityLostResponseAndExplicitReplay(t *testing.T) {
	bucketFile := creationImportFile(t, "buckets.json", `[{"sourceBucket":"src","destBucket":"dst"}]`)
	tasksFile := creationImportFile(t, "tasks.json", `[{"sourceBucket":"src","destBucket":"dst","startKey":"a","endKey":"z"}]`)
	localFile := creationImportFile(t, "local.json", `[{"direction":"LOCAL_TO_OSS","sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/source"}]},"destinationEndpoint":{"type":"OSS","s3Alias":"target","bucket":"dst"}}]`)
	oldFile := creationImportFile(t, "old.csv", "source/src,target/dst\n")
	for _, tc := range []struct {
		name string
		call func(*ApiClient, []string) (map[string]interface{}, error)
		args []string
	}{
		{"create-oss", runTaskCreate, []string{"oss-to-oss", "source/src", "target/dst"}},
		{"create-download", runTaskCreate, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/target"}},
		{"create-upload", runTaskCreate, []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/source"}},
		{"import-buckets", runTaskImport, []string{"buckets", bucketFile}},
		{"import-tasks", runTaskImport, []string{"tasks", tasksFile}},
		{"import-local", runTaskImport, []string{"local", localFile}},
		{"legacy-import", runLegacyTaskImport, []string{oldFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &acceptedCreation{dropNext: true}
			server := fixture.server(t)
			client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
			// Warm the connection, making a dropped response eligible for the
			// standard transport's automatic retry if GetBody is left enabled.
			if _, err := client.Get("/local-nodes"); err != nil {
				t.Fatal(err)
			}
			result, err := tc.call(client, tc.args)
			id, _ := result["requestId"].(string)
			if err == nil || id == "" || !strings.Contains(err.Error(), "--request-id "+id) {
				t.Fatalf("lost response must expose reusable identity: %#v, %v", result, err)
			}
			fixture.mu.Lock()
			requests, created := fixture.requests, fixture.created
			fixture.mu.Unlock()
			if requests != 1 || created != 1 {
				t.Fatalf("automatic replay: requests=%d created=%d", requests, created)
			}
			args := append(append([]string{}, tc.args...), "--request-id", id)
			result, err = tc.call(client, args)
			if err != nil || result["submissionStatus"] != "accepted" || result["requestId"] != id {
				t.Fatalf("explicit replay: %#v, %v", result, err)
			}
			fixture.mu.Lock()
			requests, created = fixture.requests, fixture.created
			fixture.mu.Unlock()
			if requests != 2 || created != 1 {
				t.Fatalf("explicit replay created a duplicate: requests=%d created=%d", requests, created)
			}
		})
	}
}

func TestCreationRequestIdentityDoesNotDeduplicateIntentionalNewTasks(t *testing.T) {
	fixture := &acceptedCreation{}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	args := []string{"oss-to-oss", "source/src", "target/dst"}
	first, err := runTaskCreate(client, args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runTaskCreate(client, args)
	if err != nil || first["requestId"] == second["requestId"] {
		t.Fatalf("new invocation reused identity: %#v %#v %v", first, second, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.created != 2 {
		t.Fatal("identical intentional task was suppressed")
	}
}

func TestCreationRequestIdentityV4NeverAutomaticallyReplays(t *testing.T) {
	fixture := &acceptedCreation{legacy: true, dropNext: true}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	_, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst"})
	if err == nil || !strings.Contains(err.Error(), "旧 V4") {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 1 || fixture.created != 1 {
		t.Fatalf("V4 POST replayed: requests=%d created=%d", fixture.requests, fixture.created)
	}
}

func TestCreationRequestIdentityValidationDoesNotReserveID(t *testing.T) {
	fixture := &acceptedCreation{}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	args := []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/source", "--request-id", "operator-request-1"}
	result, err := runTaskCreate(client, append(args, "--validate-only"))
	if err != nil || result["submissionStatus"] != "validated" || result["requestId"] != nil {
		t.Fatalf("preflight must not submit/reserve identity: %#v %v", result, err)
	}
	result, err = runTaskCreate(client, args)
	if err != nil || result["requestId"] != "operator-request-1" {
		t.Fatalf("creation after preflight: %#v %v", result, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 1 || fixture.created != 1 {
		t.Fatalf("preflight created a task: %d %d", fixture.requests, fixture.created)
	}
}

func TestCreationRequestIdentityRejectsInvalidIDBeforeNetwork(t *testing.T) {
	for _, id := range []string{"", "with space", "x\r\nheader: bad", "中文", strings.Repeat("a", 221)} {
		_, err := runTaskCreate(creationTestClient(t), []string{"oss-to-oss", "source/src", "target/dst", "--request-id", id})
		if err == nil {
			t.Errorf("accepted invalid identity %q", id)
		}
	}
	for _, call := range []func(*ApiClient, []string) (map[string]interface{}, error){runTaskCreate, runTaskImport, runLegacyTaskImport} {
		_, err := call(creationTestClient(t), []string{"--request-id", "one", "--request-id", "two"})
		if err == nil || !strings.Contains(err.Error(), "只能填写一次") {
			t.Fatal(err)
		}
	}
}

func TestCreationRequestIdentityJSONErrorIsReusable(t *testing.T) {
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "test-token")
	fixture := &acceptedCreation{dropNext: true}
	server := fixture.server(t)
	args := []string{"--host", server.URL, "--json", "task", "create", "oss-to-oss", "source/src", "target/dst"}
	var output, errors bytes.Buffer
	if code := runCLI(args, strings.NewReader(""), &output, &errors); code == 0 {
		t.Fatal("lost response reported success")
	}
	var result map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	id, _ := result["requestId"].(string)
	if id == "" || !strings.Contains(errors.String(), "--request-id "+id) || result["success"] != false {
		t.Fatalf("identity not exposed in both outputs: %s, %s", &output, &errors)
	}
	output.Reset()
	errors.Reset()
	if code := runCLI(append(args, "--request-id", id), strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("replay failed: %s %s", &output, &errors)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.created != 1 {
		t.Fatal("CLI replay created duplicate task")
	}
}

func TestCreationRequestIdentityConflictAndPartialExposeOriginalID(t *testing.T) {
	fixture := &acceptedCreation{}
	server := fixture.server(t)
	client := &ApiClient{baseUrl: server.URL + "/api/master", client: server.Client()}
	args := []string{"oss-to-oss", "source/src", "target/dst", "--request-id", "fixed-request"}
	if _, err := runTaskCreate(client, args); err != nil {
		t.Fatal(err)
	}
	result, err := runTaskCreate(client, append(args, "--prefix", "changed/"))
	if err == nil || result["requestId"] != "fixed-request" || !strings.Contains(err.Error(), "409") {
		t.Fatalf("changed payload conflict lost identity: %#v %v", result, err)
	}
	partialClient := creationTestClient(t, creationNodesExchange(), creationExchange{method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":0,"taskIds":[7],"partial":true}`})
	result, err = runTaskCreate(partialClient, args)
	if err == nil || result["requestId"] != "fixed-request" || !strings.Contains(err.Error(), "--request-id fixed-request") || result["taskIds"].([]interface{})[0] != float64(7) {
		t.Fatalf("partial response lost identity or IDs: %#v %v", result, err)
	}
}

func TestCreationRequestHasNoAutomaticReplayBody(t *testing.T) {
	client := &ApiClient{baseUrl: "http://unit.invalid/api/master", client: &http.Client{Transport: creationTransport(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil || r.Header.Get("Idempotency-Key") != "operator-id" || r.ContentLength <= 0 {
			t.Errorf("creation POST can be automatically replayed: %#v", r)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true}`)), Header: http.Header{}, Request: r}, nil
	})}}
	if _, err := client.PostCreation("/add-buckets", []interface{}{map[string]interface{}{"sourceBucket": "src"}}, "operator-id"); err != nil {
		t.Fatal(err)
	}
}
