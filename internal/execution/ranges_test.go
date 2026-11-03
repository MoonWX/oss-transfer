package execution

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// multipartTarget accepts only the multipart requests a ranged write makes.
type multipartTarget struct {
	mu      sync.Mutex
	parts   map[int][]byte
	object  []byte
	aborted int
}

func newMultipartTarget(t *testing.T, budget *transferstore.UploadMemoryBudget) (*multipartTarget, *transferstore.S3) {
	t.Helper()
	m := &multipartTarget{parts: map[int][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	store, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: 5 << 20, UploadMemory: budget, RangedReadMinSize: 1, RetryMaxAttempts: 1, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func (m *multipartTarget) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>")
	case r.Method == "PUT" && q.Has("partNumber"):
		number, _ := strconv.Atoi(q.Get("partNumber"))
		data, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.parts[number] = data
		m.mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", number))
	case r.Method == "POST" && q.Has("uploadId"):
		var body struct {
			Parts []struct{ PartNumber int } `xml:"Part"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.object = nil
		for _, part := range body.Parts {
			m.object = append(m.object, m.parts[part.PartNumber]...)
		}
		m.mu.Unlock()
		fmt.Fprint(w, "<CompleteMultipartUploadResult><ETag>\"multipart\"</ETag></CompleteMultipartUploadResult>")
	case r.Method == "DELETE":
		m.mu.Lock()
		m.aborted++
		m.mu.Unlock()
		w.WriteHeader(204)
	default:
		w.WriteHeader(400)
	}
}

// failingRange returns an interrupted read halfway through its range once.
type failingRange struct {
	io.ReadCloser
	remaining int64
}

func (r *failingRange) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	p = p[:min(int64(len(p)), r.remaining)]
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func TestRangedCopyDecoratesEachRangeAndCountsRetriedProgressOnce(t *testing.T) {
	root := t.TempDir()
	payload := make([]byte, 6*(5<<20)-777)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	budget, _ := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	server, target := newMultipartTarget(t, budget)
	stat, err := source.Stat(bg, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	item := Item{ID: "1", Source: stat, DestinationKey: "large.bin"}
	object := &objectProgress{item: ObjectProgress{Size: stat.Size}}
	var calls, peak atomic.Int64
	var mu sync.Mutex
	sizes := map[int64]int{}
	engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
		mu.Lock()
		sizes[part.Source.Size]++
		mu.Unlock()
		switch calls.Add(1) {
		case 3: // interrupted halfway
			return &failingRange{ReadCloser: input, remaining: part.Source.Size / 2}, nil
		case 5: // every byte delivered, but the connection fails instead of EOF
			return &failingRange{ReadCloser: input, remaining: part.Source.Size}, nil
		}
		return &peakReader{ReadCloser: input, object: object, peak: &peak}, nil
	}}}
	job := Job{Source: Endpoint{Kind: "LOCAL"}, Destination: Endpoint{Kind: "S3"}, ConflictPolicy: "OVERWRITE"}
	result := engine.copyItem(bg, job, item, source, target, object)
	if result.Status != ItemSucceeded {
		t.Fatalf("ranged copy failed: %+v", result)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !bytes.Equal(server.object, payload) {
		t.Fatal("target differs from source")
	}
	// Six parts and two repeated ranges; each decorated with its own length.
	if calls.Load() != 8 || sizes[5<<20] != 7 || sizes[int64(len(payload))%(5<<20)] != 1 {
		t.Fatalf("decorations %d %v", calls.Load(), sizes)
	}
	if got := object.bytes.Load(); got != stat.Size || peak.Load() > stat.Size {
		t.Fatalf("progress %d peak %d size %d", got, peak.Load(), stat.Size)
	}
}

type peakReader struct {
	io.ReadCloser
	object *objectProgress
	peak   *atomic.Int64
}

func (r *peakReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	// Counted after this read returns; sample the prior total plus this read.
	total := r.object.bytes.Load() + int64(n)
	for old := r.peak.Load(); total > old && !r.peak.CompareAndSwap(old, total); old = r.peak.Load() {
	}
	return n, err
}

func TestSmallObjectsKeepTheStreamingPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.bin"), []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	source, _ := transferstore.NewLocal(root)
	defer source.Close()
	budget, _ := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	_, target := newMultipartTarget(t, budget)
	if target.RangedWrite(5) {
		t.Fatal("single-PUT object selected ranged reads")
	}
}

func TestIdleObjectSlotsFollowAdmissionAndLiveConcurrency(t *testing.T) {
	f := newFixture(t, 3)
	entered := make(chan string, 3)
	release := make(chan struct{})
	f.startEngine(nil, Options{MaxConcurrentJobs: 2, WorkersPerJob: 2, DecorateReader: func(ctx context.Context, _ Job, item Item, input io.ReadCloser) (io.ReadCloser, error) {
		entered <- item.ID
		select {
		case <-release:
			return input, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	if idle := f.engine.IdleObjectSlots(); idle != 4 {
		t.Fatal("idle before work", idle)
	}
	f.acceptStart()
	waitAdmission(t, entered)
	waitAdmission(t, entered)
	if idle := f.engine.IdleObjectSlots(); idle != 2 {
		t.Fatal("idle with two active objects", idle)
	}
	if err := f.engine.SetConcurrency(8, 4); err != nil {
		t.Fatal(err)
	}
	waitAdmission(t, entered)
	if idle := f.engine.IdleObjectSlots(); idle != 29 {
		t.Fatal("idle after raising concurrency", idle)
	}
	if err := f.engine.SetConcurrency(1, 1); err != nil {
		t.Fatal(err)
	}
	if idle := f.engine.IdleObjectSlots(); idle != 0 {
		t.Fatal("over-admitted objects reported idle capacity", idle)
	}
	close(release)
	if record := f.wait(); record.Status != Completed {
		t.Fatalf("record %+v", record)
	}
	if idle := f.engine.IdleObjectSlots(); idle != 1 {
		t.Fatal("idle after completion", idle)
	}
}
