package transferstore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// permissionStore is a path-style S3 stand-in for one bucket that denies the
// operations named in deny (LIST, HEAD, GET, PUT, MULTIPART for creating an
// upload and its parts, ABORT, DELETE) and, when writePrefix is set, every
// write outside it, as a key limited to one prefix is.
type permissionStore struct {
	mu          sync.Mutex
	objects     map[string][]byte
	deny        map[string]bool
	writePrefix string
	aborted     int
	// uploads are the multipart uploads created and not aborted.
	uploads map[string]bool
	// ranged counts GET requests with a Range header.
	ranged int
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
		case r.Method == http.MethodDelete && query.Has("uploadId"):
			operation = "ABORT"
		case r.Method == http.MethodPost && query.Has("uploads"), r.Method == http.MethodPut && query.Has("uploadId"):
			operation = "MULTIPART"
		case r.Method == http.MethodPut:
			operation = "PUT"
		case r.Method == http.MethodDelete:
			operation = "DELETE"
		}
		outside := (operation == "PUT" || operation == "MULTIPART") && !strings.HasPrefix(key, p.writePrefix)
		if p.deny[operation] || outside {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
			return
		}
		switch {
		case operation == "LIST":
			// The first key in lexicographic order, as S3 lists them.
			var names []string
			for name := range p.objects {
				if !strings.Contains(name, PermissionProbePrefix) {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			if len(names) == 0 {
				io.WriteString(w, `<?xml version="1.0"?><ListBucketResult><Name>bucket</Name><KeyCount>0</KeyCount></ListBucketResult>`)
				return
			}
			io.WriteString(w, `<?xml version="1.0"?><ListBucketResult><Name>bucket</Name><KeyCount>1</KeyCount><Contents><Key>`+names[0]+`</Key><Size>`+strconv.Itoa(len(p.objects[names[0]]))+`</Size></Contents></ListBucketResult>`)
		case operation == "HEAD" || operation == "GET":
			body, ok := p.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
			if operation == "GET" && r.Header.Get("Range") != "" {
				p.ranged++
				if len(body) == 0 {
					// As AWS S3 and MinIO answer a range of an empty object.
					w.Header().Set("Content-Range", "bytes */0")
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					io.WriteString(w, `<?xml version="1.0"?><Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message></Error>`)
					return
				}
				w.Header().Set("Content-Range", "bytes 0-0/"+strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body[:1])
			} else if operation == "GET" {
				w.Write(body)
			}
		case operation == "PUT":
			if r.Header.Get("If-None-Match") != "*" {
				t.Errorf("probe PUT is not conditional")
			}
			body, _ := io.ReadAll(r.Body)
			p.objects[key] = body
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		case operation == "MULTIPART" && r.Method == http.MethodPost:
			if p.uploads == nil {
				p.uploads = map[string]bool{}
			}
			p.uploads[key] = true
			io.WriteString(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>`+key+`</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case operation == "MULTIPART" && r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		case operation == "ABORT":
			p.aborted++
			delete(p.uploads, key)
			w.WriteHeader(http.StatusNoContent)
		case operation == "DELETE":
			delete(p.objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func checkPermissions(t *testing.T, store *permissionStore, write bool, prefix ...string) PermissionCheck {
	t.Helper()
	server := httptest.NewServer(store.handler(t))
	t.Cleanup(server.Close)
	check, err := CheckBucketPermissions(context.Background(), S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()}, write, strings.Join(prefix, ""))
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
	if len(store.objects) != 1 || store.aborted != 1 || len(store.uploads) != 0 || check.LeftoverKey != "" || check.LeftoverUploadKey != "" {
		t.Fatalf("probe left behind: objects %v, aborted %d, uploads %v, leftover %q %q", store.objects, store.aborted, store.uploads, check.LeftoverKey, check.LeftoverUploadKey)
	}
	if got := permissionStatuses(check); got[PermissionMultipartAbort] != PermissionPassed || len(got) != 8 {
		t.Fatalf("statuses %v", got)
	}
}

func TestBucketPermissionCheckNamesEachDeniedOperation(t *testing.T) {
	// Read-only keys on an empty bucket: reads cannot be sampled, writes fail.
	store := &permissionStore{objects: map[string][]byte{}, deny: map[string]bool{"PUT": true, "MULTIPART": true}}
	check := checkPermissions(t, store, true)
	got := permissionStatuses(check)
	if got[PermissionList] != PermissionPassed || got[PermissionHead] != PermissionSkipped || got[PermissionPut] != PermissionFailed || got[PermissionHeadWritten] != PermissionSkipped || got[PermissionMultipart] != PermissionFailed || got[PermissionMultipartAbort] != PermissionSkipped {
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
	check, err := CheckBucketPermissions(context.Background(), S3Config{Endpoint: server.URL, Bucket: "missing", AccessKey: "test", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	got := permissionStatuses(check)
	if got[PermissionList] != PermissionFailed || got[PermissionPut] != PermissionSkipped || check.Results[len(check.Results)-1].Reason != PermissionReasonNoSuchBucket {
		t.Fatalf("results %+v", check.Results)
	}
}

// Folder markers and _SUCCESS files are empty and sort before the objects
// next to them. Reading such a first object must not fail: a range of an
// empty object is refused with 416, so it is read whole, and the result
// names the object it read.
func TestBucketPermissionCheckReadsAnEmptyFirstObject(t *testing.T) {
	store := &permissionStore{objects: map[string][]byte{"logs/": {}, "logs/_SUCCESS": {}, "logs/part-0000": []byte("abc")}}
	check := checkPermissions(t, store, false)
	got := permissionStatuses(check)
	if got[PermissionHead] != PermissionPassed || got[PermissionGet] != PermissionPassed || store.ranged != 0 {
		t.Fatalf("statuses %v, ranged GETs %d: %+v", got, store.ranged, check.Results)
	}
	for _, result := range check.Results {
		if (result.Operation == PermissionHead || result.Operation == PermissionGet) && result.Key != "logs/" {
			t.Fatalf("%s read %q", result.Operation, result.Key)
		}
	}
	// A non-empty object is still read one byte at a time.
	store = &permissionStore{objects: map[string][]byte{"logs/part-0000": []byte("abc")}}
	if got := permissionStatuses(checkPermissions(t, store, false)); got[PermissionGet] != PermissionPassed || store.ranged != 1 {
		t.Fatalf("statuses %v, ranged GETs %d", got, store.ranged)
	}
}

// Writing does not need AbortMultipartUpload: PutObject covers creating and
// uploading parts. A denied abort is its own row, so it cannot fail the
// multipart row, and the upload it leaves behind is named.
func TestBucketPermissionCheckReportsADeniedAbortSeparately(t *testing.T) {
	store := &permissionStore{objects: map[string][]byte{"a": []byte("abc")}, deny: map[string]bool{"ABORT": true}}
	check := checkPermissions(t, store, true)
	got := permissionStatuses(check)
	for _, operation := range []string{PermissionPut, PermissionHeadWritten, PermissionMultipart, PermissionDeleteProbe} {
		if got[operation] != PermissionPassed {
			t.Fatalf("%s: statuses %v", operation, got)
		}
	}
	if got[PermissionMultipartAbort] != PermissionFailed || len(store.uploads) != 1 || !store.uploads[check.LeftoverUploadKey] || check.LeftoverUploadID != "upload-1" {
		t.Fatalf("abort: statuses %v, uploads %v, leftover %q %q", got, store.uploads, check.LeftoverUploadKey, check.LeftoverUploadID)
	}
	if check.Results[len(check.Results)-2].Operation != PermissionMultipartAbort {
		t.Fatalf("abort is not reported after the multipart upload: %+v", check.Results)
	}
}

// A key that may only write below a prefix is checked there: the probes are
// written below the requested prefix and removed afterwards.
func TestBucketPermissionCheckWritesBelowTheRequestedPrefix(t *testing.T) {
	store := &permissionStore{objects: map[string][]byte{"a": []byte("abc")}, writePrefix: "backup/"}
	check := checkPermissions(t, store, true)
	if got := permissionStatuses(check); got[PermissionPut] != PermissionFailed || got[PermissionMultipart] != PermissionFailed || got[PermissionMultipartAbort] != PermissionSkipped || check.Prefix != "" {
		t.Fatalf("bucket root: statuses %v, prefix %q", got, check.Prefix)
	}
	check = checkPermissions(t, store, true, "backup/")
	for operation, status := range permissionStatuses(check) {
		if status != PermissionPassed {
			t.Errorf("%s: %s", operation, status)
		}
	}
	for _, result := range check.Results {
		if result.Operation != PermissionList && result.Operation != PermissionHead && result.Operation != PermissionGet && !strings.HasPrefix(result.Key, "backup/"+PermissionProbePrefix) {
			t.Errorf("%s wrote %q", result.Operation, result.Key)
		}
	}
	if check.Prefix != "backup/" || len(store.objects) != 1 || len(store.uploads) != 0 {
		t.Fatalf("prefix %q, objects %v, uploads %v", check.Prefix, store.objects, store.uploads)
	}
	// A read-only check does not claim a write prefix.
	if check := checkPermissions(t, store, false, "backup/"); check.Prefix != "" {
		t.Fatalf("read-only check names prefix %q", check.Prefix)
	}
	for _, prefix := range []string{"bad\nprefix", strings.Repeat("p", MaxPermissionPrefix+1), "\xff"} {
		if _, err := CheckBucketPermissions(context.Background(), S3Config{Bucket: "bucket"}, true, prefix); err == nil {
			t.Errorf("prefix %q accepted", prefix)
		}
	}
}
