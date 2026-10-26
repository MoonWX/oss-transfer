package master

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type takeoverStorage struct {
	mu                         sync.Mutex
	source, target             map[string][]byte
	reads, writes              map[string]int
	readStarted, readCancelled chan struct{}
	listStarted, listCancelled chan struct{}
	readOnce, listOnce         sync.Once
}

func (s *takeoverStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	old := r.Header.Get("X-Test-Owner") == "old"
	if bucket == "source" && r.Method == http.MethodGet && key == "" {
		start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
		if old && start > 0 {
			s.listOnce.Do(func() { close(s.listStarted) })
			<-r.Context().Done()
			select {
			case <-s.listCancelled:
			default:
				close(s.listCancelled)
			}
			return
		}
		type entry struct {
			Key, ETag    string
			Size         int
			LastModified string
		}
		result := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			Name                  string
			IsTruncated           bool
			NextContinuationToken string  `xml:"NextContinuationToken,omitempty"`
			Contents              []entry `xml:"Contents"`
		}{Name: bucket}
		end := min(start+128, len(s.source))
		for number := start; number < end; number++ {
			name := fmt.Sprintf("objects/%03d", number)
			payload := s.source[name]
			sum := md5.Sum(payload)
			result.Contents = append(result.Contents, entry{Key: name, Size: len(payload), ETag: `"` + hex.EncodeToString(sum[:]) + `"`, LastModified: "2026-01-01T00:00:00Z"})
		}
		if end < len(s.source) {
			result.IsTruncated, result.NextContinuationToken = true, strconv.Itoa(end)
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	if bucket == "source" && r.Method == http.MethodGet && old && key == "objects/001" {
		s.readOnce.Do(func() { close(s.readStarted) })
		<-r.Context().Done()
		select {
		case <-s.readCancelled:
		default:
			close(s.readCancelled)
		}
		return
	}
	if bucket == "target" && r.Method == http.MethodPut {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		s.mu.Lock()
		_, exists := s.target[key]
		if exists && r.Header.Get("If-None-Match") == "*" {
			s.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.target[key] = payload
		s.writes[key]++
		s.mu.Unlock()
		sum := md5.Sum(payload)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mu.Lock()
	var payload []byte
	var found bool
	if bucket == "source" {
		payload, found = s.source[key]
	} else if bucket == "target" {
		payload, found = s.target[key]
	}
	if bucket == "source" && r.Method == http.MethodGet {
		s.reads[key]++
	}
	s.mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sum := md5.Sum(payload)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == http.MethodGet {
		_, _ = w.Write(payload)
	}
}

type takeoverOwnerTransport struct {
	base  http.RoundTripper
	owner string
}

func (t takeoverOwnerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("X-Test-Owner", t.owner)
	return t.base.RoundTrip(copy)
}

func labelTakeoverAgent(agent *takeoverAgent, owner string) {
	base := agent.service.Factory.HTTPClientForEndpoint
	agent.service.Factory.HTTPClientForEndpoint = func(ctx context.Context, endpoint execution.Endpoint, config storageconfig.Config) *http.Client {
		client := *base(ctx, endpoint, config)
		client.Transport = takeoverOwnerTransport{base: client.Transport, owner: owner}
		return &client
	}
}

// The storage fixture serves actual S3 wire requests and stored bytes. Scanner,
// scheduler, transfer engine, database and authenticated Agent RPC are real.
// Blocking the old owner's GET and second LIST page makes both cancellation
// and recovery observable without manufacturing completed child records.
func TestFailoverScannerAndInFlightTransferFinishWithIdenticalBytes(t *testing.T) {
	f := newAgentHTTPFixture(t)
	storage := &takeoverStorage{source: map[string][]byte{}, target: map[string][]byte{}, reads: map[string]int{}, writes: map[string]int{},
		readStarted: make(chan struct{}), readCancelled: make(chan struct{}), listStarted: make(chan struct{}), listCancelled: make(chan struct{})}
	for i := 0; i < 256; i++ {
		storage.source[fmt.Sprintf("objects/%03d", i)] = bytes.Repeat([]byte(fmt.Sprintf("payload-%03d", i)), 16)
	}
	s3 := httptest.NewServer(storage)
	t.Cleanup(s3.Close)
	a, b := takeoverTestAgent(t, f, "data-a"), takeoverTestAgent(t, f, "data-b")
	labelTakeoverAgent(a, "old")
	labelTakeoverAgent(b, "new")
	if err := a.service.Engine.SetConcurrency(1, 1); err != nil {
		t.Fatal(err)
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{
		"alias": "qa", "endpoint": s3.URL, "accessKey": "fixture-ak", "secretKey": "fixture-sk", "region": "test", "pathStyleEnable": true, "validate": false,
	})
	if code != http.StatusOK {
		t.Fatal("storage setup", code, body)
	}
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl, runtime.leases.margin = time.Second, 100*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		lastErr = runtime.step(ctx, p.ID)
		current, err := runtime.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Counters.Succeeded == 1 && current.DispatchedThrough == 1 {
			p = current
			break
		}
		if ctx.Err() != nil {
			t.Fatal("old owner did not report partial progress", current, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, signal := range []<-chan struct{}{storage.readStarted, storage.listStarted} {
		select {
		case <-signal:
		case <-ctx.Done():
			t.Fatal("old scanner and reader were not both in flight")
		}
	}
	ownerCtx, err := a.service.Leases.ParentContext(p.ID, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	a.cancel() // Disconnect only the control channel; leave data-plane services alive.
	select {
	case <-ownerCtx.Done():
	case <-ctx.Done():
		t.Fatal("disconnected owner's lease did not cancel its execution")
	}
	for _, signal := range []<-chan struct{}{storage.readCancelled, storage.listCancelled} {
		select {
		case <-signal:
		case <-ctx.Done():
			t.Fatal("lease cancellation did not abort the real S3 request")
		}
	}
	for !p.NodeRetired(a.node.ID) {
		p, err = runtime.reconcileFailover(ctx, p, map[string]bool{b.node.ID: true})
		if err != nil {
			t.Fatal("takeover preparation", err)
		}
		if ctx.Err() != nil {
			t.Fatal("expired node was not retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.ScannerNode().ID != b.node.ID || !p.Failover.ScannerSeeding {
		t.Fatal("scanner ownership was not transferred", p)
	}
	for p.Status != "COMPLETED" {
		lastErr = runtime.step(ctx, p.ID)
		p, err = runtime.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil || p.Status == "FAILED" || p.Status == "PARTIAL_FAILED" {
			t.Fatal("takeover did not finish", p, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.ScannedObjects != 256 || p.Counters.Succeeded != 256 || p.Counters.Skipped != 0 || p.Counters.Failed != 0 || p.DispatchedThrough != 2 {
		t.Fatal("takeover counters or scanner batches changed", p)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.target) != len(storage.source) || storage.reads["objects/000"] != 1 || storage.writes["objects/000"] != 1 {
		t.Fatal("missing targets or retransmitted credited object", len(storage.target), storage.reads["objects/000"], storage.writes["objects/000"])
	}
	for key, source := range storage.source {
		if sha256.Sum256(source) != sha256.Sum256(storage.target[key]) {
			t.Fatal("recovered target bytes differ", key)
		}
	}
}
