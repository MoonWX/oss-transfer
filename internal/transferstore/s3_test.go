package transferstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This server exercises wire requests, retries, aborts, and integrity using a
// deliberately limited S3 protocol simulation. It is not JD OSS compatibility
// acceptance; that must run separately against an actual object storage service.
type protocolObject struct {
	data    []byte
	etag    string
	headers http.Header
}
type protocolS3 struct {
	mu                         sync.Mutex
	objects                    map[string]protocolObject
	parts                      map[int][]byte
	partAttempts               map[int]int
	uploaded                   [][]byte
	initHeaders                http.Header
	completed, aborted, puts   int
	signed, conditional, match bool
	v2Signed                   bool
	failFirstPart, collision   bool
	blockPart                  bool
	abortDenied                bool
	onPart                     func(int)
	partStarted                chan struct{}
	server                     *httptest.Server
}

var protocolTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func objectETag(data []byte) string {
	sum := sha256.Sum256(data)
	return "\"" + hex.EncodeToString(sum[:]) + "\""
}
func newProtocolS3(t *testing.T) *protocolS3 {
	t.Helper()
	p := &protocolS3{objects: map[string]protocolObject{}, parts: map[int][]byte{}, partAttempts: map[int]int{}, partStarted: make(chan struct{}, 1)}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}
func (p *protocolS3) store(t *testing.T) *S3 {
	t.Helper()
	s, err := NewS3(S3Config{Endpoint: p.server.URL, Region: "test-region", Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 1, RetryMaxAttempts: 2, HTTPClient: p.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func protocolError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>test response</Message></Error>", code)
}
func (p *protocolS3) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.v2Signed = p.v2Signed || strings.HasPrefix(r.Header.Get("Authorization"), "AWS test-access:")
	p.signed = p.signed || strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/")
	p.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/test-bucket") {
		protocolError(w, 400, "InvalidBucket")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	q := r.URL.Query()
	if r.Method == "GET" && q.Get("list-type") == "2" {
		p.mu.Lock()
		defer p.mu.Unlock()
		keys := []string{}
		for key := range p.objects {
			if strings.HasPrefix(key, q.Get("prefix")) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		start, _ := strconv.Atoi(q.Get("continuation-token"))
		if q.Get("continuation-token") == "" && q.Get("start-after") != "" {
			start = sort.Search(len(keys), func(i int) bool { return keys[i] > q.Get("start-after") })
		} else if q.Get("continuation-token") != "" && q.Get("start-after") != "" {
			protocolError(w, 400, "InvalidArgument")
			return
		}
		limit, _ := strconv.Atoi(q.Get("max-keys"))
		if limit < 1 {
			limit = 1000
		}
		end := min(start+limit, len(keys))
		type entry struct {
			Key          string
			Size         int
			ETag         string
			LastModified string
		}
		type listing struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string `xml:",omitempty"`
			Contents              []entry
		}
		result := listing{IsTruncated: end < len(keys)}
		if result.IsTruncated {
			result.NextContinuationToken = strconv.Itoa(end)
		}
		for _, key := range keys[start:end] {
			obj := p.objects[key]
			result.Contents = append(result.Contents, entry{key, len(obj.data), obj.etag, protocolTime.Format(time.RFC3339)})
		}
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	if r.Method == "POST" && q.Has("uploads") {
		p.mu.Lock()
		p.initHeaders = r.Header.Clone()
		p.parts = map[int][]byte{}
		p.mu.Unlock()
		fmt.Fprint(w, "<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>object</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>")
		return
	}
	if r.Method == "DELETE" && q.Get("uploadId") != "" {
		p.mu.Lock()
		p.aborted++
		if p.abortDenied {
			p.mu.Unlock()
			protocolError(w, 403, "AccessDenied")
			return
		}
		p.parts = map[int][]byte{}
		p.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if r.Method == "GET" && q.Get("uploadId") != "" {
		p.mu.Lock()
		aborted := p.aborted > 0
		p.mu.Unlock()
		if aborted {
			protocolError(w, 404, "NoSuchUpload")
		} else {
			protocolError(w, 501, "NotImplemented")
		}
		return
	}
	if r.Method == "PUT" && q.Get("uploadId") != "" {
		number, _ := strconv.Atoi(q.Get("partNumber"))
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.partAttempts[number]++
		attempt := p.partAttempts[number]
		p.uploaded = append(p.uploaded, append([]byte(nil), data...))
		block, fail := p.blockPart, p.failFirstPart
		p.mu.Unlock()
		if p.onPart != nil {
			p.onPart(number)
		}
		if block {
			select {
			case p.partStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		if fail && number == 1 && attempt == 1 {
			protocolError(w, 503, "SlowDown")
			return
		}
		p.mu.Lock()
		p.parts[number] = data
		p.mu.Unlock()
		w.Header().Set("ETag", objectETag(data))
		w.WriteHeader(200)
		return
	}
	if r.Method == "POST" && q.Get("uploadId") != "" {
		var body struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&body); err != nil {
			protocolError(w, 400, "InvalidRequest")
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.conditional = r.Header.Get("If-None-Match") == "*"
		if p.collision {
			p.objects[key] = protocolObject{data: []byte("concurrent"), etag: "\"concurrent\""}
			protocolError(w, 412, "PreconditionFailed")
			return
		}
		var result []byte
		for index, part := range body.Parts {
			if part.PartNumber != index+1 || part.ETag != objectETag(p.parts[part.PartNumber]) {
				protocolError(w, 400, "InvalidPart")
				return
			}
			result = append(result, p.parts[part.PartNumber]...)
		}
		etag := "\"multipart-etag-not-an-md5\""
		p.objects[key] = protocolObject{data: result, etag: etag, headers: p.initHeaders}
		p.completed++
		w.Header().Set("x-amz-version-id", "v1")
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><ETag>%s</ETag></CompleteMultipartUploadResult>", etag)
		return
	}
	if r.Method == "PUT" {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.puts++
		p.conditional = r.Header.Get("If-None-Match") == "*"
		if _, exists := p.objects[key]; exists && p.conditional {
			protocolError(w, 412, "PreconditionFailed")
			return
		}
		etag := objectETag(data)
		p.objects[key] = protocolObject{data: data, etag: etag, headers: r.Header.Clone()}
		w.Header().Set("ETag", etag)
		w.Header().Set("x-amz-version-id", "v1")
		return
	}
	if r.Method == "HEAD" || r.Method == "GET" {
		p.mu.Lock()
		obj, exists := p.objects[key]
		p.match = r.Header.Get("If-Match") == obj.etag && obj.etag != ""
		p.mu.Unlock()
		if !exists {
			protocolError(w, 404, "NoSuchKey")
			return
		}
		if value := r.Header.Get("If-Match"); value != "" && value != obj.etag {
			protocolError(w, 412, "PreconditionFailed")
			return
		}
		for header, values := range obj.headers {
			if strings.HasPrefix(strings.ToLower(header), "x-amz-meta-") || header == "Content-Type" || header == "Cache-Control" || header == "Content-Encoding" || header == "Content-Disposition" {
				for _, value := range values {
					w.Header().Add(header, value)
				}
			}
		}
		w.Header().Set("ETag", obj.etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.Header().Set("Last-Modified", protocolTime.Format(http.TimeFormat))
		w.Header().Set("x-amz-version-id", "v1")
		if r.Method == "GET" {
			_, _ = w.Write(obj.data)
		}
		return
	}
	protocolError(w, 400, "UnsupportedTestRequest")
}

func TestS3ProtocolSingleWriteReadAndListing(t *testing.T) {
	p := newProtocolS3(t)
	s := p.store(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("content\x00汉字"), 300)
	keys := []string{"literal/../key with space", "prefix/a", "prefix/b", "prefix/c"}
	for _, key := range keys {
		result, err := s.Write(ctx, key, bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), ContentType: "application/octet-stream", CacheControl: "max-age=60", Metadata: map[string]string{"source": "test"}})
		if err != nil || !result.Committed || result.Object.ETag != objectETag(payload) || result.Object.VersionID != "v1" {
			t.Fatalf("put %q: %+v %v", key, result, err)
		}
	}
	object, err := s.Stat(ctx, keys[0])
	if err != nil || object.Metadata["source"] != "test" || object.ContentType != "application/octet-stream" || object.CacheControl != "max-age=60" {
		t.Fatalf("metadata %+v %v", object, err)
	}
	reader, _, err := s.Open(ctx, keys[0], OpenOptions{Expected: &object})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || sha256.Sum256(data) != sha256.Sum256(payload) {
		t.Fatalf("GET integrity %v", err)
	}
	p.mu.Lock()
	signed, conditional, match := p.signed, p.conditional, p.match
	p.mu.Unlock()
	if !signed || !conditional || !match {
		t.Fatalf("wire auth/conditional pin missing %t %t %t", signed, conditional, match)
	}
	result, err := s.Write(ctx, keys[0], panicReader{}, WriteOptions{Size: 3})
	if err != nil || !result.Skipped {
		t.Fatalf("skip %+v %v", result, err)
	}
	first, err := s.List(ctx, ListOptions{Prefix: "prefix/", Limit: 1})
	if err != nil || len(first.Objects) != 1 || first.Objects[0].Key != "prefix/a" || first.NextCursor == "" {
		t.Fatalf("list %+v %v", first, err)
	}
	second, err := s.List(ctx, ListOptions{Prefix: "prefix/", Limit: 1, Cursor: first.NextCursor})
	if err != nil || second.Objects[0].Key != "prefix/b" {
		t.Fatalf("second %+v %v", second, err)
	}
	if _, err = s.List(ctx, ListOptions{Prefix: "different", Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor accepted wrong prefix %v", err)
	}
	var scanned []string
	if err = s.Scan(ctx, ListOptions{Prefix: "prefix/", Limit: 1}, func(o Object) error { scanned = append(scanned, o.Key); return nil }); err != nil || strings.Join(scanned, ",") != "prefix/a,prefix/b,prefix/c" {
		t.Fatalf("scan %v %v", scanned, err)
	}
	first, err = s.List(ctx, ListOptions{Prefix: "prefix/", StartAfter: "prefix/a", Limit: 1})
	if err != nil || len(first.Objects) != 1 || first.Objects[0].Key != "prefix/b" || first.NextCursor == "" {
		t.Fatalf("start-after first page %+v %v", first, err)
	}
	second, err = s.List(ctx, ListOptions{Prefix: "prefix/", StartAfter: "prefix/a", Cursor: first.NextCursor, Limit: 1})
	if err != nil || len(second.Objects) != 1 || second.Objects[0].Key != "prefix/c" {
		t.Fatalf("start-after continuation %+v %v", second, err)
	}
	if _, err = s.List(ctx, ListOptions{Prefix: "prefix/", Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor accepted changed start-after %v", err)
	}
	scanned = nil
	if err = s.Scan(ctx, ListOptions{Prefix: "prefix/", StartAfter: "prefix/a", Limit: 1}, func(o Object) error { scanned = append(scanned, o.Key); return nil }); err != nil || strings.Join(scanned, ",") != "prefix/b,prefix/c" {
		t.Fatalf("start-after scan %v %v", scanned, err)
	}
	stale := object
	stale.ETag = "\"stale\""
	if _, _, err = s.Open(ctx, keys[0], OpenOptions{Expected: &stale}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("stale GET accepted %v", err)
	}
	stale = object
	stale.Size++
	if _, _, err = s.Open(ctx, keys[0], OpenOptions{Expected: &stale}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("mismatched content length accepted %v", err)
	}
}

func TestS3ProtocolMultipartRetryIntegrity(t *testing.T) {
	p := newProtocolS3(t)
	p.failFirstPart = true
	s := p.store(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), int(minPartSize/16)+97)
	result, err := s.Write(context.Background(), "multipart", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), Metadata: map[string]string{"source": "multipart"}})
	if err != nil || !result.Committed || result.Object.ETag != "\"multipart-etag-not-an-md5\"" {
		t.Fatalf("multipart %+v %v", result, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if sha256.Sum256(p.objects["multipart"].data) != sha256.Sum256(payload) || p.completed != 1 || p.aborted != 0 || p.partAttempts[1] != 2 || p.partAttempts[2] != 1 || !p.conditional {
		t.Fatalf("multipart data/state completed%d abort%d attempts%v", p.completed, p.aborted, p.partAttempts)
	}
	if !bytes.Equal(p.uploaded[0], p.uploaded[1]) {
		t.Fatal("retry did not replay the same part")
	}
	if p.objects["multipart"].headers.Get("x-amz-meta-source") != "multipart" {
		t.Fatal("multipart lost metadata")
	}
}

func TestS3ProtocolRejectLengthAndAbort(t *testing.T) {
	for _, name := range []string{"short-small", "long-small", "short-multipart", "long-multipart"} {
		t.Run(name, func(t *testing.T) {
			p := newProtocolS3(t)
			s := p.store(t)
			size := int64(4)
			if strings.Contains(name, "multipart") {
				size = minPartSize + 10
			}
			actual := size - 1
			if strings.HasPrefix(name, "long") {
				actual = size + 1
			}
			data := bytes.Repeat([]byte("x"), int(actual))
			result, err := s.Write(context.Background(), "target", bytes.NewReader(data), WriteOptions{Size: size, Overwrite: true})
			if !errors.Is(err, ErrSizeMismatch) || result.Committed {
				t.Fatalf("length accepted %+v %v", result, err)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.completed != 0 || p.puts != 0 {
				t.Fatal("invalid source published")
			}
			if strings.Contains(name, "multipart") && p.aborted != 1 {
				t.Fatalf("multipart not aborted: %d", p.aborted)
			}
		})
	}
}

func TestS3ProtocolCancellationAbortsUsingFreshContext(t *testing.T) {
	p := newProtocolS3(t)
	p.blockPart = true
	s := p.store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := bytes.Repeat([]byte("x"), int(minPartSize+1))
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(ctx, "cancel", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), Overwrite: true})
		done <- err
	}()
	select {
	case <-p.partStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("part upload did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SDK upload did not cancel")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted != 1 || p.completed != 0 {
		t.Fatalf("cancel failed cleanup abort%d complete%d", p.aborted, p.completed)
	}
}

func TestS3ProtocolConcurrentSkipAtMultipartCommit(t *testing.T) {
	p := newProtocolS3(t)
	p.collision = true
	s := p.store(t)
	payload := bytes.Repeat([]byte("x"), int(minPartSize+1))
	result, err := s.Write(context.Background(), "collision", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload))})
	if err != nil || !result.Skipped || result.Committed {
		t.Fatalf("concurrent skip %+v %v", result, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(p.objects["collision"].data) != "concurrent" || p.aborted != 1 {
		t.Fatalf("concurrent object overwritten or upload leaked")
	}
}

func TestS3RejectUnsupportedConfiguration(t *testing.T) {
	config := S3Config{Bucket: "bucket", AccessKey: "a", SecretKey: "secret", SignatureVersion: "v7"}
	if _, err := NewS3(config); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("accepted unknown signature version: %v", err)
	}
	config.SignatureVersion = "v4"
	config.PartConcurrency = 17
	if _, err := NewS3(config); err == nil {
		t.Fatalf("ignored concurrency: %v", err)
	}
	config.PartConcurrency = 1
	config.Endpoint = "https://user:secret@host.invalid"
	if _, err := NewS3(config); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("endpoint rejected without exposing credentials: %v", err)
	}
}

type repeatingReader byte

func (r repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestLargePartSpoolsAndCleansWithoutWholePartAllocation(t *testing.T) {
	directory := t.TempDir()
	size := maxMemoryPart + 1
	body, err := readPart(io.LimitReader(repeatingReader('q'), size), size, directory)
	if err != nil {
		t.Fatal(err)
	}
	if body.file == nil {
		t.Fatal("large part unexpectedly buffered in memory")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, body.reader)
	if err != nil || count != size {
		t.Fatalf("spool read %d %v", count, err)
	}
	expected := sha256.New()
	_, _ = io.Copy(expected, io.LimitReader(repeatingReader('q'), size))
	if !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("spooling changed data")
	}
	if err = body.close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("spool leaked %v", err)
	}
	if _, err = readPart(strings.NewReader("short"), size, directory); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short disk part accepted %v", err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("failed spool leaked %v", err)
	}
}

func TestS3PermissionDeniedIsNotTreatedAsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { protocolError(w, 403, "AccessDenied") }))
	defer server.Close()
	store, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", Region: "test", AccessKey: "test", SecretKey: "test", PathStyle: true, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Stat(context.Background(), "key"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("access denied became missing: %v", err)
	}
	if _, err = store.Write(context.Background(), "key", panicReader{}, WriteOptions{Size: 10}); err == nil {
		t.Fatal("SKIP treated denied HEAD as missing")
	}
}

type countedReader struct {
	reader io.Reader
	count  atomic.Int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count.Add(int64(n))
	return n, err
}

func TestMultipartConcurrencyAndBackpressure(t *testing.T) {
	p := newProtocolS3(t)
	arrivals := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p.onPart = func(int) { arrivals <- struct{}{}; <-release }
	store, err := NewS3(S3Config{Endpoint: p.server.URL, Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 3, MaxBufferedBytes: 15 * 1024 * 1024, HTTPClient: p.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("p"), int(4*minPartSize+7))
	input := &countedReader{reader: bytes.NewReader(payload)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := store.Write(ctx, "parallel", input, WriteOptions{Size: int64(len(payload)), Overwrite: true})
		if err == nil && !result.Committed {
			err = errors.New("write not committed")
		}
		done <- err
	}()
	for range 3 {
		select {
		case <-arrivals:
		case <-ctx.Done():
			t.Fatal("three parts did not run concurrently")
		}
	}
	if read := input.count.Load(); read != 3*minPartSize {
		t.Fatalf("source read beyond three buffered slots: %d", read)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !bytes.Equal(p.objects["parallel"].data, payload) || p.completed != 1 || len(p.parts) != 5 {
		t.Fatal("parallel upload did not complete all parts in source order")
	}
}

func TestListBucketsPaginationAndPermissionFailure(t *testing.T) {
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS ") {
			protocolError(w, 403, "AccessDenied")
			return
		}
		pages.Add(1)
		if r.URL.Query().Get("continuation-token") == "next" {
			fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>second</Name></Bucket></Buckets></ListAllMyBucketsResult>")
			return
		}
		fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>first</Name></Bucket></Buckets><ContinuationToken>next</ContinuationToken></ListAllMyBucketsResult>")
	}))
	defer server.Close()
	config := S3Config{Endpoint: server.URL, AccessKey: "fake", SecretKey: "fake-secret", SignatureVersion: "v2", HTTPClient: server.Client()}
	names, err := ListBuckets(context.Background(), config)
	if err != nil || strings.Join(names, ",") != "first,second" || pages.Load() != 2 {
		t.Fatalf("list buckets %+v %v (%d pages)", names, err, pages.Load())
	}
	config.SignatureVersion = "v4"
	if _, err = ListBuckets(context.Background(), config); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("permission failure swallowed: %v", err)
	}
}
