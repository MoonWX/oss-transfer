package agentservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// The write check writes below the requested prefix, so a key limited to
// that prefix can be checked; an invalid prefix is refused before anything
// is written.
func TestStoragePermissionsWritesBelowTheRequestedPrefix(t *testing.T) {
	var mu sync.Mutex
	var written []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bucket"), "/")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		switch {
		case r.Method == http.MethodGet && key == "":
			io.WriteString(w, `<?xml version="1.0"?><ListBucketResult><Name>bucket</Name><KeyCount>0</KeyCount></ListBucketResult>`)
		case r.Method == http.MethodPost:
			mu.Lock()
			written = append(written, key)
			mu.Unlock()
			io.WriteString(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>`+key+`</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			mu.Lock()
			written = append(written, key)
			mu.Unlock()
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	service := &Service{Factory: &storageconfig.Factory{HTTPClientForEndpoint: func(context.Context, execution.Endpoint, storageconfig.Config) *http.Client { return server.Client() }}}
	run := func(prefix string) agenttransport.Response {
		t.Helper()
		payload, _ := json.Marshal(StoragePermissionsRequest{Config: storageconfig.Config{Alias: "s3", Endpoint: server.URL, AccessKey: "key", SecretKey: "secret"}, Bucket: "bucket", Write: true, Prefix: prefix})
		response, err := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "storage_permissions", Payload: payload}})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := run("team-a/")
	var check transferstore.PermissionCheck
	if response.Status != 200 || json.Unmarshal(response.Payload, &check) != nil || check.Prefix != "team-a/" {
		t.Fatalf("check: %+v", response)
	}
	mu.Lock()
	if len(written) == 0 {
		t.Fatal("nothing was written")
	}
	for _, key := range written {
		if !strings.HasPrefix(key, "team-a/"+transferstore.PermissionProbePrefix) {
			t.Errorf("wrote %q outside the prefix", key)
		}
	}
	written = nil
	mu.Unlock()
	if response := run("bad\nprefix"); response.Status != 400 {
		t.Fatalf("invalid prefix: %+v", response)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(written) != 0 {
		t.Fatalf("an invalid prefix wrote %v", written)
	}
}
