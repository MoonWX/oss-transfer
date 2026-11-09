package transferstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// outageS3 is an S3 target that can go down: while drops remain, a request
// loses its connection without a response, half-way through its body or
// after it, as when a storage service is stopped. It stores PUTs and
// multipart uploads in memory.
type outageS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	uploads  map[string]map[int][]byte
	created  int
	versions map[string]int
	cuts     int // GET bodies to end half-way
	requests int
	drops    int // < 0: drop every request
	server   *httptest.Server
}

func newOutageS3(t *testing.T) *outageS3 {
	t.Helper()
	s := &outageS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, versions: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *outageS3) drop() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if s.drops == 0 {
		return false, false
	}
	if s.drops > 0 {
		s.drops--
	}
	return true, s.requests%2 == 0
}

func (s *outageS3) setDrops(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops, s.requests = n, 0
}

func (s *outageS3) serve(w http.ResponseWriter, r *http.Request) {
	if dropped, midBody := s.drop(); dropped {
		if midBody && r.ContentLength > 1 {
			_, _ = io.CopyN(io.Discard, r.Body, r.ContentLength/2)
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	if r.Method == http.MethodGet {
		s.serveGet(w, r, key)
		return
	}
	query := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		s.created++
		id := "upload-" + strconv.Itoa(s.created)
		s.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == http.MethodPut && query.Has("uploadId"):
		parts := s.uploads[query.Get("uploadId")]
		number, _ := strconv.Atoi(query.Get("partNumber"))
		if parts == nil || number < 1 {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchUpload</Code></Error>")
			return
		}
		parts[number] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, number))
	case r.Method == http.MethodPost && query.Has("uploadId"):
		parts := s.uploads[query.Get("uploadId")]
		var request struct {
			Parts []struct {
				PartNumber int
			} `xml:"Part"`
		}
		if parts == nil || xml.Unmarshal(body, &request) != nil {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchUpload</Code></Error>")
			return
		}
		var object []byte
		for _, part := range request.Parts {
			object = append(object, parts[part.PartNumber]...)
		}
		s.objects[key] = object
		delete(s.uploads, query.Get("uploadId"))
		io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && query.Has("uploadId"):
		delete(s.uploads, query.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		s.objects[key] = body
		s.versions[key]++
		w.Header().Set("ETag", `"object"`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// serveGet serves an object with its version as ETag, honouring Range and
// If-Match. While cuts remain, a body ends half-way. Bodies are written
// without the lock: a client may hold one open while it writes.
func (s *outageS3) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	data, ok := s.objects[key]
	version := s.versions[key]
	cut := s.cuts > 0
	if cut {
		s.cuts--
	}
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
		return
	}
	etag := fmt.Sprintf(`"v%d"`, version)
	if match := r.Header.Get("If-Match"); match != "" && match != etag {
		w.WriteHeader(http.StatusPreconditionFailed)
		io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
		return
	}
	w.Header().Set("ETag", etag)
	status, start := http.StatusOK, 0
	if spec, found := strings.CutPrefix(r.Header.Get("Range"), "bytes="); found {
		first, last, _ := strings.Cut(spec, "-")
		start, _ = strconv.Atoi(first)
		end, err := strconv.Atoi(last)
		if err != nil {
			end = len(data) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data, status = data[start:end+1], http.StatusPartialContent
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if cut {
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	w.Write(data)
}

// outageNames resolves made-up host names to the test server, and stops
// resolving them while lookups are failing, like the name of a stopped
// container. Every request dials, so every request looks its host up.
type outageNames struct {
	address string
	failing atomic.Int32 // lookups left to fail; < 0: all
	dials   atomic.Int32
}

func (n *outageNames) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		n.dials.Add(1)
		host, _, _ := net.SplitHostPort(address)
		if left := n.failing.Load(); left != 0 {
			if left > 0 {
				n.failing.Add(-1)
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, n.address)
	}}}
}

var outageHostCount atomic.Int32

// store connects to the server under a host name no other test has used, so
// whether this process reached it before is up to the test.
func (s *outageS3) store(t *testing.T) (*S3, *outageNames) {
	t.Helper()
	names := &outageNames{address: s.server.Listener.Addr().String()}
	_, port, _ := net.SplitHostPort(names.address)
	host := fmt.Sprintf("outage-%d.storage.test", outageHostCount.Add(1))
	store, err := NewS3(S3Config{Endpoint: "http://" + host + ":" + port, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: time.Millisecond, HTTPClient: names.client()})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the SDK's own backoff for ordinary errors out of the test's time.
	store.retryer.standard = retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = 3
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return time.Millisecond, nil })
		o.RateLimiter = store.retryer.quota
	})
	return store, names
}

func outagePayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	return data
}

func (s *outageS3) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key]
}

func (s *outageS3) openUploads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []string{}
	for id := range s.uploads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *outageS3) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// A storage service that is stopped and started again ends the connections of
// requests in flight (EOF, reset) and refuses new ones. More dropped requests
// than the three ordinary attempts must not fail an object: like refused
// connections, they get the ten attempts the documentation promises.
func TestS3WritesRideOutDroppedConnections(t *testing.T) {
	server := newOutageS3(t)
	store, _ := server.store(t)
	write := func(key string, data []byte) error {
		_, err := store.Write(context.Background(), key, bytes.NewReader(data), WriteOptions{Size: int64(len(data)), Overwrite: true})
		return err
	}
	// The storage answered before the outage.
	if err := write("before", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	small, large := outagePayload(1<<20), outagePayload(int(2*minPartSize+minPartSize/2))
	for _, test := range []struct {
		key  string
		data []byte
	}{{"small", small}, {"large", large}} {
		// Nine dropped requests: an attempt of any one request can be the
		// tenth at most.
		server.setDrops(9)
		if err := write(test.key, test.data); err != nil {
			t.Fatalf("%s: %v", test.key, err)
		}
		if !bytes.Equal(server.object(test.key), test.data) {
			t.Fatalf("%s: stored object differs", test.key)
		}
		if requests := server.requestCount(); requests <= 9 {
			t.Fatalf("%s: only %d requests", test.key, requests)
		}
	}
	if uploads := server.openUploads(); len(uploads) != 0 {
		t.Fatalf("multipart uploads left open: %v", uploads)
	}
}

func TestS3WriteFailsWhenTheOutageOutlastsTheRetries(t *testing.T) {
	server := newOutageS3(t)
	store, _ := server.store(t)
	if _, err := store.Write(context.Background(), "before", strings.NewReader("ok"), WriteOptions{Size: 2, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	server.setDrops(-1)
	data := outagePayload(1 << 20)
	_, err := store.Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(len(data)), Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 10") || server.requestCount() != 10 {
		t.Fatalf("persistent outage: %d requests, %v", server.requestCount(), err)
	}
	if server.object("object") != nil {
		t.Fatal("an object was stored during the outage")
	}
}

// A connection that ends without a response from a host that never answered
// is more likely a wrong Endpoint or addressing style than an outage: it keeps
// the three ordinary attempts.
func TestS3DroppedConnectionsFromANewHostFailFast(t *testing.T) {
	server := newOutageS3(t)
	store, _ := server.store(t)
	server.setDrops(-1)
	_, err := store.Write(context.Background(), "object", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
	if err == nil || server.requestCount() != 3 {
		t.Fatalf("new host: %d requests, %v", server.requestCount(), err)
	}
}

// The name of a stopped container stops resolving until it starts again.
// That is an outage for a host that answered before, and a configuration
// error for one that never did.
func TestS3HostNamesThatStopResolving(t *testing.T) {
	server := newOutageS3(t)
	store, names := server.store(t)
	names.failing.Store(-1)
	_, err := store.Write(context.Background(), "unknown", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "no such host") || names.dials.Load() != 1 {
		t.Fatalf("a name that never resolved must fail at once: %d lookups, %v", names.dials.Load(), err)
	}
	names.failing.Store(0)
	if _, err := store.Write(context.Background(), "before", strings.NewReader("ok"), WriteOptions{Size: 2, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	names.failing.Store(7)
	names.dials.Store(0)
	if _, err := store.Write(context.Background(), "after", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true}); err != nil || names.dials.Load() != 8 {
		t.Fatalf("a name that resolved before: %d lookups, %v", names.dials.Load(), err)
	}
	if string(server.object("after")) != "payload" {
		t.Fatal("object not stored after the name resolved again")
	}
	names.failing.Store(-1)
	names.dials.Store(0)
	_, err = store.Write(context.Background(), "gone", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 10") || names.dials.Load() != 10 {
		t.Fatalf("a name gone for good: %d lookups, %v", names.dials.Load(), err)
	}
}

// Retries reach the service log, at most one line per interval, and the
// line after a quiet interval counts the retries it did not write.
func TestS3RetriesAreLoggedAndRateLimited(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	SetRetryLogger(func(line string) { mu.Lock(); lines = append(lines, line); mu.Unlock() })
	defer SetRetryLogger(nil)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	retryLog.Lock()
	retryLog.now = func() time.Time { return now }
	retryLog.Unlock()
	defer func() { retryLog.Lock(); retryLog.now = time.Now; retryLog.Unlock() }()
	server := newOutageS3(t)
	store, _ := server.store(t)
	if _, err := store.Write(context.Background(), "before", strings.NewReader("ok"), WriteOptions{Size: 2, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	server.setDrops(4)
	if _, err := store.Write(context.Background(), "object", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(lines) != 1 || !strings.Contains(lines[0], "storage request PutObject on bucket bucket: attempt 1 of 10 failed (transient failure), retrying: ") || strings.Contains(lines[0], "x-id=") {
		t.Fatalf("retry lines: %q", lines)
	}
	mu.Unlock()
	retryLog.Lock()
	now = now.Add(retryLogInterval)
	retryLog.Unlock()
	server.setDrops(1)
	if _, err := store.Write(context.Background(), "next", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || !strings.Contains(lines[1], "(and 3 more storage request retries since the last report)") {
		t.Fatalf("retry lines: %q", lines)
	}
}

// A source that is stopped while an object is read ends the body half-way
// and drops the next requests. The rest of the object is read from where the
// body ended once the source is back, pinned to the version first read.
func TestS3ReadsResumeAfterTheSourceComesBack(t *testing.T) {
	server := newOutageS3(t)
	store, _ := server.store(t)
	data := outagePayload(3 << 20)
	if _, err := store.Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(len(data)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	read := func() ([]byte, error) {
		reader, _, err := store.Open(context.Background(), "object", OpenOptions{})
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	}
	setFaults := func(cuts, drops int) {
		server.mu.Lock()
		server.cuts, server.drops = cuts, drops
		server.mu.Unlock()
	}
	// The first body ends half-way and so does the second: each resume
	// made progress.
	setFaults(2, 0)
	server.mu.Lock()
	server.requests = 0
	server.mu.Unlock()
	got, err := read()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("interrupted read: %d of %d bytes, %v", len(got), len(data), err)
	}
	if requests := server.requestCount(); requests != 3 {
		t.Fatalf("interrupted read took %d GETs, want 3", requests)
	}
	// A resume that finds the source down retries like any request.
	setFaults(1, 0)
	reader, _, err := store.Open(context.Background(), "object", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server.setDrops(8)
	got, err = io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read across an outage: %d of %d bytes, %v", len(got), len(data), err)
	}
	// An object replaced while it was read is not mixed with its new version.
	setFaults(1, 0)
	reader, _, err = store.Open(context.Background(), "object", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(len(data)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(reader)
	reader.Close()
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed source: %v", err)
	}
	// Bodies that keep ending are resumed while each brings bytes, and
	// given up after the attempts a range read has once they bring none.
	setFaults(100, 0)
	server.mu.Lock()
	server.requests = 0
	server.mu.Unlock()
	if _, err := read(); err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("endless cuts: %v", err)
	}
}
