package transferstore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// permissionStore is a path-style S3 stand-in for one bucket that denies the
// operations named in deny.
type permissionStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deny    map[string]bool
	aborted int
}

func (p *permissionStore) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/bucket")
		key = strings.TrimPrefix(key, "/")
		query := r.URL.Query()
		operation := ""
		switch {
		case r.Method == http.MethodGet && key == "":
			operation = "LIST"
		case r.Method == http.MethodHead:
			operation = "HEAD"
		case r.Method == http.MethodGet:
			operation = "GET"
		case r.Method == http.MethodPost && query.Has("uploads"), r.Method == http.MethodPut && query.Has("uploadId"), r.Method == http.MethodDelete && query.Has("uploadId"):
			operation = "MULTIPART"
		case r.Method == http.MethodPut:
			operation = "PUT"
		case r.Method == http.MethodDelete:
			operation = "DELETE"
		}
		if p.deny[operation] {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
			return
		}
		switch {
		case operation == "LIST":
			io.WriteString(w, `<?xml version="1.0"?><ListBucketResult><Name>bucket</Name><KeyCount>`)
			if len(p.objects) == 0 {
				io.WriteString(w, `0</KeyCount></ListBucketResult>`)
				return
			}
			for name := range p.objects {
				if !strings.HasPrefix(name, PermissionProbePrefix) {
					io.WriteString(w, `1</KeyCount><Contents><Key>`+name+`</Key><Size>3</Size></Contents></ListBucketResult>`)
					return
				}
			}
			io.WriteString(w, `0</KeyCount></ListBucketResult>`)
		case operation == "HEAD" || operation == "GET":
			body, ok := p.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
			if operation == "GET" {
				w.Header().Set("Content-Range", "bytes 0-0/3")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body[:1])
			}
		case operation == "PUT":
			if r.Header.Get("If-None-Match") != "*" {
				t.Errorf("probe PUT is not conditional")
			}
			body, _ := io.ReadAll(r.Body)
			p.objects[key] = body
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		case operation == "MULTIPART" && r.Method == http.MethodPost:
			io.WriteString(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>`+key+`</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case operation == "MULTIPART" && r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		case operation == "MULTIPART":
			p.aborted++
			w.WriteHeader(http.StatusNoContent)
		case operation == "DELETE":
			delete(p.objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func checkPermissions(t *testing.T, store *permissionStore, write bool) PermissionCheck {
	t.Helper()
	server := httptest.NewServer(store.handler(t))
	t.Cleanup(server.Close)
	check, err := CheckBucketPermissions(context.Background(), S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()}, write)
	if err != nil {
		t.Fatal(err)
	}
	return check
}

func permissionStatuses(check PermissionCheck) map[string]string {
	statuses := map[string]string{}
	for _, result := range check.Results {
		statuses[result.Operation] = result.Status
	}
	return statuses
}

func TestBucketPermissionCheckReadsWithoutWriting(t *testing.T) {
	store := &permissionStore{objects: map[string][]byte{"photos/a.jpg": []byte("abc")}}
	check := checkPermissions(t, store, false)
	want := map[string]string{PermissionList: PermissionPassed, PermissionHead: PermissionPassed, PermissionGet: PermissionPassed, PermissionPut: PermissionSkipped, PermissionHeadWritten: PermissionSkipped, PermissionMultipart: PermissionSkipped}
	if got := permissionStatuses(check); len(got) != len(want) || got[PermissionList] != want[PermissionList] || got[PermissionGet] != want[PermissionGet] || got[PermissionPut] != PermissionSkipped || got[PermissionMultipart] != PermissionSkipped {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	if len(store.objects) != 1 || store.aborted != 0 {
		t.Fatalf("a read-only check wrote to the bucket: %v", store.objects)
	}
}

func TestBucketPermissionCheckWritesAndRemovesItsProbe(t *testing.T) {
	store := &permissionStore{objects: map[string][]byte{"photos/a.jpg": []byte("abc")}}
	check := checkPermissions(t, store, true)
	for operation, status := range permissionStatuses(check) {
		if status != PermissionPassed {
			t.Errorf("%s: %s", operation, status)
		}
	}
	if len(store.objects) != 1 || store.aborted != 1 || check.LeftoverKey != "" {
		t.Fatalf("probe left behind: objects %v, aborted %d, leftover %q", store.objects, store.aborted, check.LeftoverKey)
	}
}

func TestBucketPermissionCheckNamesEachDeniedOperation(t *testing.T) {
	// Read-only keys on an empty bucket: reads cannot be sampled, writes fail.
	store := &permissionStore{objects: map[string][]byte{}, deny: map[string]bool{"PUT": true, "MULTIPART": true}}
	check := checkPermissions(t, store, true)
	got := permissionStatuses(check)
	if got[PermissionList] != PermissionPassed || got[PermissionHead] != PermissionSkipped || got[PermissionPut] != PermissionFailed || got[PermissionHeadWritten] != PermissionSkipped || got[PermissionMultipart] != PermissionFailed {
		t.Fatalf("statuses %v", got)
	}
	for _, result := range check.Results {
		if result.Status == PermissionFailed && result.ErrorKind != StorageErrorAccessDenied {
			t.Fatalf("%s failed as %q, not access denied: %s", result.Operation, result.ErrorKind, result.Error)
		}
		if result.Operation == PermissionHead && result.Reason != PermissionReasonEmptyBucket {
			t.Fatalf("HEAD skipped for %q", result.Reason)
		}
	}
	// A probe that cannot be deleted is named so the operator can remove it.
	store = &permissionStore{objects: map[string][]byte{"a": []byte("abc")}, deny: map[string]bool{"DELETE": true}}
	check = checkPermissions(t, store, true)
	if check.LeftoverKey == "" || store.objects[check.LeftoverKey] == nil || !strings.HasPrefix(check.LeftoverKey, PermissionProbePrefix) {
		t.Fatalf("leftover %q, objects %v", check.LeftoverKey, store.objects)
	}
}

// A missing bucket is never written to: some stores would create it.
func TestBucketPermissionCheckDoesNotWriteToAMissingBucket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("wrote to a missing bucket: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchBucket</Code><Message>missing</Message></Error>`)
	}))
	defer server.Close()
	check, err := CheckBucketPermissions(context.Background(), S3Config{Endpoint: server.URL, Bucket: "missing", AccessKey: "test", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := permissionStatuses(check)
	if got[PermissionList] != PermissionFailed || got[PermissionPut] != PermissionSkipped || check.Results[len(check.Results)-1].Reason != PermissionReasonNoSuchBucket {
		t.Fatalf("results %+v", check.Results)
	}
}
