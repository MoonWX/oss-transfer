package comparison

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// engineS3Object is one object of the engineS3 fixture.
type engineS3Object struct {
	data        []byte
	etag        string // quoted; "" sends no ETag
	contentType string
	metadata    map[string]string
}

// engineS3 serves path-style S3 LIST, HEAD and GET (with ranges and If-Match)
// for the real AWS SDK. LIST reports LastModified as listTime; HEAD and GET
// report Last-Modified as headTime.
type engineS3 struct {
	mu       sync.Mutex
	objects  map[string]engineS3Object // "bucket/key"
	counts   map[string]int            // "LIST bucket", "HEAD bucket/key", "GET bucket/key"
	ranges   []string                  // "bucket/key bytes=a-b"
	listTime string
	headTime string
	// fault answers a request instead of the fixture when it returns true.
	// count numbers the requests with the same operation and object from 1.
	fault  func(w http.ResponseWriter, r *http.Request, operation, object string, count int) bool
	server *httptest.Server
}

func newEngineS3(t *testing.T) *engineS3 {
	t.Helper()
	s := &engineS3{objects: map[string]engineS3Object{}, counts: map[string]int{}, listTime: "2026-01-01T00:00:00.123Z", headTime: "Thu, 01 Jan 2026 00:00:00 GMT"}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *engineS3) put(object, data string) {
	sum := md5.Sum([]byte(data))
	s.putObject(object, engineS3Object{data: []byte(data), etag: `"` + hex.EncodeToString(sum[:]) + `"`})
}

func (s *engineS3) putObject(object string, value engineS3Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[object] = value
}

func (s *engineS3) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[name]
}

// store opens bucket through the real S3 client. SDK retries are disabled so
// the engine's own retries are what the tests observe.
func (s *engineS3) store(t *testing.T, bucket string) *transferstore.S3 {
	t.Helper()
	return engineS3Store(t, s.server.URL, bucket, s.server.Client())
}

func engineS3Store(t *testing.T, endpoint, bucket string, client *http.Client) *transferstore.S3 {
	t.Helper()
	store, err := transferstore.NewS3(transferstore.S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, AccessKey: "engine-test-access", SecretKey: "engine-test-secret", PathStyle: true, RetryMaxAttempts: 1, ThrottleRetryMaxAttempts: 1, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func engineS3Error(w http.ResponseWriter, r *http.Request, status int, code, requestID string) {
	w.Header().Set("X-Amz-Request-Id", requestID)
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>test error</Message><RequestId>%s</RequestId></Error>`, code, requestID)
}

func (s *engineS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	operation, object := r.Method, bucket+"/"+key
	if r.Method == http.MethodGet && key == "" {
		operation, object = "LIST", bucket
	}
	s.mu.Lock()
	s.counts[operation+" "+object]++
	count := s.counts[operation+" "+object]
	if value := r.Header.Get("Range"); value != "" {
		s.ranges = append(s.ranges, object+" "+value)
	}
	s.mu.Unlock()
	if s.fault != nil && s.fault(w, r, operation, object, count) {
		return
	}
	switch operation {
	case "LIST":
		s.list(w, r, bucket)
	case http.MethodHead, http.MethodGet:
		s.object(w, r, object)
	default:
		engineS3Error(w, r, 400, "UnsupportedTestRequest", "REQ-UNSUPPORTED")
	}
}

func (s *engineS3) list(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	if query.Get("list-type") != "2" {
		engineS3Error(w, r, 400, "UnsupportedTestRequest", "REQ-LIST")
		return
	}
	prefix, after := query.Get("prefix"), query.Get("start-after")
	if token := query.Get("continuation-token"); token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			engineS3Error(w, r, 400, "InvalidArgument", "REQ-TOKEN")
			return
		}
		after = string(decoded)
	}
	maxKeys, _ := strconv.Atoi(query.Get("max-keys"))
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	type entry struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag,omitempty"`
		Size         int    `xml:"Size"`
	}
	var result struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string   `xml:"Name"`
		Prefix                string   `xml:"Prefix"`
		KeyCount              int      `xml:"KeyCount"`
		MaxKeys               int      `xml:"MaxKeys"`
		IsTruncated           bool     `xml:"IsTruncated"`
		NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
		Contents              []entry  `xml:"Contents"`
	}
	result.Name, result.Prefix, result.MaxKeys = bucket, prefix, maxKeys
	s.mu.Lock()
	var keys []string
	for name := range s.objects {
		if key, ok := strings.CutPrefix(name, bucket+"/"); ok && strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(result.Contents) == maxKeys {
			result.IsTruncated = true
			result.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(result.Contents[maxKeys-1].Key))
			break
		}
		object := s.objects[bucket+"/"+key]
		result.Contents = append(result.Contents, entry{Key: key, LastModified: s.listTime, ETag: object.etag, Size: len(object.data)})
	}
	s.mu.Unlock()
	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func (s *engineS3) object(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.Lock()
	object, ok := s.objects[name]
	s.mu.Unlock()
	if !ok {
		engineS3Error(w, r, 404, "NoSuchKey", "REQ-NOSUCHKEY")
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && object.etag != "" && match != object.etag {
		engineS3Error(w, r, 412, "PreconditionFailed", "REQ-412")
		return
	}
	header := w.Header()
	if object.etag != "" {
		header.Set("ETag", object.etag)
	}
	header.Set("Last-Modified", s.headTime)
	contentType := object.contentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	for key, value := range object.metadata {
		header.Set("X-Amz-Meta-"+key, value)
	}
	body, status := object.data, http.StatusOK
	if value := r.Header.Get("Range"); value != "" && r.Method == http.MethodGet {
		first, last, _ := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
		start, err1 := strconv.Atoi(first)
		end, err2 := strconv.Atoi(last)
		if err1 != nil || err2 != nil || start > end || start >= len(object.data) {
			engineS3Error(w, r, 416, "InvalidRange", "REQ-416")
			return
		}
		end = min(end, len(object.data)-1)
		body, status = object.data[start:end+1], http.StatusPartialContent
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(object.data)))
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func TestEngineS3MillisecondListTimesStillMatch(t *testing.T) {
	for _, etags := range []bool{true, false} {
		for _, mode := range []string{"metadata", "content"} {
			t.Run(fmt.Sprintf("etags=%v/%s", etags, mode), func(t *testing.T) {
				fixture := newEngineS3(t)
				for name, data := range map[string]string{"source-bucket/src/a": "aaaa", "target-bucket/backup/a": "aaaa", "source-bucket/src/b": "bbbbbb", "target-bucket/backup/b": "bbbbbb", "source-bucket/src/c": "cc"} {
					if etags {
						fixture.put(name, data)
					} else {
						fixture.putObject(name, engineS3Object{data: []byte(data)})
					}
				}
				// The LIST time is 123 ms into the second HEAD reports.
				r := request()
				r.Mode = mode
				rows, err := engineRun(context.Background(), r, fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
				if err != nil {
					t.Fatal(err)
				}
				got := engineRowsByKey(t, rows)
				if got["a"].Status != StatusMatch || got["b"].Status != StatusMatch || got["c"].Status != StatusMissingTarget || len(rows) != 3 {
					t.Fatalf("millisecond LIST times changed the result: %+v", rows)
				}
				if mode == "content" && (got["b"].SourceSHA256 != engineSHA256([]byte("bbbbbb")) || fixture.count("GET source-bucket/src/b") != 1) {
					t.Fatalf("content was not compared: %+v", got["b"])
				}
			})
		}
	}
}

func TestEngineS3MetadataReasonsAndETagMismatch(t *testing.T) {
	fixture := newEngineS3(t)
	fixture.put("source-bucket/src/same", "same-data")
	fixture.put("target-bucket/backup/same", "same-data")
	fixture.put("source-bucket/src/etag", "content-A")
	fixture.put("target-bucket/backup/etag", "content-B")
	fixture.put("source-bucket/src/upper", "same-data")
	sameSum := md5.Sum([]byte("same-data"))
	fixture.putObject("target-bucket/backup/upper", engineS3Object{data: []byte("same-data"), etag: `"` + strings.ToUpper(hex.EncodeToString(sameSum[:])) + `"`})
	fixture.putObject("source-bucket/src/multipart", engineS3Object{data: []byte("part-A"), etag: `"9b2cf535f27731c974343645a3985328-2"`})
	fixture.putObject("target-bucket/backup/multipart", engineS3Object{data: []byte("part-B"), etag: `"6b1b36cbb04b41490bfc0ab2bfa26f86-2"`})
	fixture.putObject("source-bucket/src/props", engineS3Object{data: []byte("props"), etag: `"a"`, contentType: "text/plain", metadata: map[string]string{"owner": "a"}})
	fixture.putObject("target-bucket/backup/props", engineS3Object{data: []byte("props"), etag: `"a"`, contentType: "text/html", metadata: map[string]string{"owner": "b"}})
	fixture.put("source-bucket/src/size", "short")
	fixture.put("target-bucket/backup/size", "longer")
	want := map[string]map[string]string{
		"metadata": {"same": StatusMatch, "etag": StatusETagMismatch + ":" + ReasonETag, "upper": StatusMatch, "multipart": StatusMatch, "props": StatusDifferent + ":" + ReasonContentType + "," + ReasonMetadata, "size": StatusDifferent + ":" + ReasonSize},
		"content":  {"same": StatusMatch, "etag": StatusDifferent + ":" + ReasonSHA256, "upper": StatusMatch, "multipart": StatusDifferent + ":" + ReasonSHA256, "props": StatusMatch, "size": StatusDifferent + ":" + ReasonSize},
	}
	for mode, outcomes := range want {
		t.Run(mode, func(t *testing.T) {
			r := request()
			r.Mode = mode
			rows, err := engineRun(context.Background(), r, fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := engineRowsByKey(t, rows)
			for key, outcome := range outcomes {
				if engineOutcome(got[key]) != outcome {
					t.Fatalf("%s: got %+v, want %s", key, got[key], outcome)
				}
			}
		})
	}
}

func TestEngineS3ListRetriesTransientFailures(t *testing.T) {
	engineFastRetries(t)
	fixture := newEngineS3(t)
	fixture.put("source-bucket/src/a", "a")
	fixture.put("target-bucket/backup/a", "a")
	fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, object string, count int) bool {
		if operation != "LIST" || object != "source-bucket" {
			return false
		}
		switch count {
		case 1:
			engineS3Error(w, r, 500, "InternalError", "REQ-500")
		case 2:
			engineS3Error(w, r, 503, "SlowDown", "REQ-503")
		default:
			return false
		}
		return true
	}
	rows, err := engineRun(context.Background(), request(), fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || fixture.count("LIST source-bucket") != 3 {
		t.Fatalf("LIST 500 and 503 were not retried: %v %+v after %d LISTs", err, rows, fixture.count("LIST source-bucket"))
	}
}

func TestEngineS3DefinitiveListErrorsFailAtOnce(t *testing.T) {
	engineFastRetries(t)
	for _, test := range []struct {
		bucket, code string
		status       int
		want         string
	}{
		{"source-bucket", "AccessDenied", 403, "source LIST: HTTP 403 AccessDenied (request ID REQ-DEFINITIVE)"},
		{"source-bucket", "InvalidAccessKeyId", 403, "source LIST: HTTP 403 InvalidAccessKeyId (request ID REQ-DEFINITIVE)"},
		{"source-bucket", "SignatureDoesNotMatch", 403, "source LIST: HTTP 403 SignatureDoesNotMatch (request ID REQ-DEFINITIVE)"},
		{"target-bucket", "NoSuchBucket", 404, "target LIST: HTTP 404 NoSuchBucket (request ID REQ-DEFINITIVE)"},
	} {
		t.Run(test.code, func(t *testing.T) {
			fixture := newEngineS3(t)
			fixture.put("source-bucket/src/a", "a")
			fixture.put("target-bucket/backup/a", "a")
			fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, object string, _ int) bool {
				if operation == "LIST" && object == test.bucket {
					engineS3Error(w, r, test.status, test.code, "REQ-DEFINITIVE")
					return true
				}
				return false
			}
			rows, err := engineRun(context.Background(), request(), fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
			if err == nil || ErrorDetail(err) != test.want || len(rows) != 0 {
				t.Fatalf("got %q (%v) with %d rows, want %q", ErrorDetail(err), err, len(rows), test.want)
			}
			if count := fixture.count("LIST " + test.bucket); count != 1 {
				t.Fatalf("a definitive LIST error was retried: %d LISTs", count)
			}
			if strings.Contains(ErrorDetail(err), "127.0.0.1") || strings.Contains(ErrorDetail(err), "engine-test") {
				t.Fatalf("detail disclosed the endpoint or credentials: %q", ErrorDetail(err))
			}
		})
	}
}

func TestEngineS3HeadErrorsCarryDetails(t *testing.T) {
	fixture := newEngineS3(t)
	fixture.put("source-bucket/src/both", "data")
	fixture.put("target-bucket/backup/both", "data")
	fixture.put("source-bucket/src/source-only", "data")
	fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, object string, _ int) bool {
		if operation == http.MethodHead && strings.HasPrefix(object, "target-bucket/") {
			engineS3Error(w, r, 403, "AccessDenied", "REQ-HEAD")
			return true
		}
		return false
	}
	rows, err := engineRun(context.Background(), request(), fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := engineRowsByKey(t, rows)
	// A HEAD error response has no body, so the SDK names the status.
	const detail = "target HEAD: HTTP 403 Forbidden (request ID REQ-HEAD)"
	if row := got["both"]; row.Status != StatusError || row.Error != "Cannot read object metadata" || row.ErrorDetail != detail {
		t.Fatalf("HEAD error on both sides: %+v", row)
	}
	if row := got["source-only"]; row.Status != StatusError || row.Error != "Cannot read target metadata" || row.ErrorDetail != detail {
		t.Fatalf("HEAD error confirming a missing target: %+v", row)
	}
}

func TestEngineS3RangesRetryBrokenAndStalledBodies(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	engineOverride(t, &readIdleTimeout, 300*time.Millisecond)
	data := enginePayload(300<<10, 5)
	fixture := newEngineS3(t)
	sum := md5.Sum(data)
	object := engineS3Object{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`}
	fixture.putObject("source-bucket/src/large", object)
	fixture.putObject("target-bucket/backup/large", object)
	var stalled atomic.Int64
	fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, name string, _ int) bool {
		if operation != http.MethodGet {
			return false
		}
		value := r.Header.Get("Range")
		fixture.mu.Lock()
		repeated := 0
		for _, entry := range fixture.ranges {
			if entry == name+" "+value {
				repeated++
			}
		}
		fixture.mu.Unlock()
		if repeated != 1 {
			return false
		}
		switch {
		case name == "source-bucket/src/large" && value == "bytes=131072-196607":
			// The connection closes after half of the declared body.
			w.Header().Set("ETag", object.etag)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 131072-196607/%d", len(data)))
			w.Header().Set("Content-Length", "65536")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[131072 : 131072+32768])
			return true
		case name == "target-bucket/backup/large" && value == "bytes=0-65535":
			// The body stops after 1,000 bytes until the client gives up.
			w.Header().Set("ETag", object.etag)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-65535/%d", len(data)))
			w.Header().Set("Content-Length", "65536")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[:1000])
			w.(http.Flusher).Flush()
			stalled.Add(1)
			<-r.Context().Done()
			return true
		}
		return false
	}
	var read atomic.Int64
	r := request()
	r.Mode = "content"
	rows, err := engineRun(context.Background(), r, fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{OnBytes: func(n int64) { read.Add(n) }})
	want := engineSHA256(data)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || rows[0].SourceSHA256 != want || rows[0].DestinationSHA256 != want {
		t.Fatalf("ranged retries produced a wrong result: %v %+v", err, rows)
	}
	fixture.mu.Lock()
	ranges := slices.Clone(fixture.ranges)
	fixture.mu.Unlock()
	for _, retry := range []string{"source-bucket/src/large bytes=163840-196607", "target-bucket/backup/large bytes=1000-65535"} {
		if !slices.Contains(ranges, retry) {
			t.Fatalf("missing continuation %q in %v", retry, ranges)
		}
	}
	if stalled.Load() != 1 || read.Load() != 2*int64(len(data)) {
		t.Fatalf("stalled %d times, read %d bytes, want 1 and %d", stalled.Load(), read.Load(), 2*len(data))
	}
}

func TestEngineS3NetworkErrorDetailHidesTheEndpoint(t *testing.T) {
	engineFastRetries(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var connections atomic.Int64
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = connection.Close()
		}
	}()
	endpoint := "http://" + listener.Addr().String()
	source := engineS3Store(t, endpoint, "source-bucket", &http.Client{Transport: &http.Transport{}})
	target := engineS3Store(t, endpoint, "target-bucket", &http.Client{Transport: &http.Transport{}})
	_, err = engineRun(context.Background(), request(), source, target, nil, RunOptions{})
	detail := ErrorDetail(err)
	if detail != "source LIST: network error" {
		t.Fatalf("detail = %q (%v)", detail, err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("test premise: the raw error should name the endpoint: %v", err)
	}
	if connections.Load() < int64(1+len(listRetryDelays)) {
		t.Fatalf("network errors were not retried: %d connections", connections.Load())
	}
}

func TestEngineS3HeadTimeout(t *testing.T) {
	engineOverride(t, &comparisonHeadTimeout, time.Second)
	fixture := newEngineS3(t)
	fixture.put("source-bucket/src/a", "a")
	fixture.put("target-bucket/backup/a", "a")
	fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, _ string, _ int) bool {
		if operation == http.MethodHead && strings.HasPrefix(r.URL.Path, "/target-bucket/") {
			<-r.Context().Done()
			return true
		}
		return false
	}
	rows, err := engineRun(context.Background(), request(), fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
	// The limit also applies to the source HEAD; only the target part must time out.
	if err != nil || len(rows) != 1 || rows[0].Status != StatusError || !strings.HasSuffix(rows[0].ErrorDetail, "target HEAD: timeout") {
		t.Fatalf("HEAD timeout: %v %+v", err, rows)
	}
}
