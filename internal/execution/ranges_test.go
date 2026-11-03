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
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// multipartTarget accepts only the multipart requests a ranged write makes.
type multipartTarget struct {
	mu                sync.Mutex
	parts             map[int][]byte
	object            []byte
	aborted           int
	latency           time.Duration
	putActive, putMax int
}

func newMultipartTarget(t *testing.T, budget *transferstore.UploadMemoryBudget) (*multipartTarget, *transferstore.S3) {
	return newLendingTarget(t, budget, nil, 0)
}

// newLendingTarget delays each UploadPart by latency, so concurrent parts
// overlap and putMax shows how many slots a write used.
func newLendingTarget(t *testing.T, budget *transferstore.UploadMemoryBudget, lender *transferstore.PartLender, latency time.Duration) (*multipartTarget, *transferstore.S3) {
	t.Helper()
	m := &multipartTarget{parts: map[int][]byte{}, latency: latency}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	store, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: 5 << 20, UploadMemory: budget, PartLender: lender, RangedReadMinSize: 1, RetryMaxAttempts: 1, HTTPClient: server.Client()})
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
		m.mu.Lock()
		m.putActive++
		m.putMax = max(m.putMax, m.putActive)
		m.mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		time.Sleep(m.latency)
		m.mu.Lock()
		m.putActive--
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

// gatedReader models a task limiter that grants nothing until gate closes.
type gatedReader struct {
	io.ReadCloser
	ctx  context.Context
	gate <-chan struct{}
	// reading, when set, receives one value on the first Read: by then the
	// writer has reserved the memory for the part it is filling.
	reading chan<- struct{}
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.reading != nil {
		r.once.Do(func() { r.reading <- struct{}{} })
	}
	select {
	case <-r.gate:
		return r.ReadCloser.Read(p)
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func rangedSource(t *testing.T, names ...string) (*transferstore.Local, []byte) {
	t.Helper()
	root := t.TempDir()
	payload := make([]byte, 12*(5<<20))
	for i := range payload {
		payload[i] = byte(i % 253)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), payload, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source, payload
}

func idlePartLender(idle int) *transferstore.PartLender {
	lender := transferstore.NewPartLender()
	lender.SetIdle(func() int { return idle })
	return lender
}

func TestThrottledTaskStreamsAndLeavesSharedMemoryToOthers(t *testing.T) {
	source, payload := rangedSource(t, "throttled.bin", "open.bin")
	// Room for four 5 MiB parts: a ranged write blocked on its task limiter
	// would hold all of it in base slots alone.
	budget, _ := transferstore.NewUploadMemoryBudget(4 * (5 << 20))
	lender := idlePartLender(100)
	slowServer, slowTarget := newLendingTarget(t, budget, lender, 0)
	openServer, openTarget := newLendingTarget(t, budget, lender, 20*time.Millisecond)
	gate := make(chan struct{})
	entered := make(chan string, 16)
	reading := make(chan struct{}, 1)
	var mu sync.Mutex
	var throttledSizes []int64
	engine := &Engine{opts: Options{
		TaskThrottled: func(job Job) bool { return job.ParentID == 1 },
		DecorateReader: func(ctx context.Context, job Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
			if job.ParentID != 1 {
				return input, nil
			}
			mu.Lock()
			throttledSizes = append(throttledSizes, part.Source.Size)
			mu.Unlock()
			entered <- part.ID
			return &gatedReader{ReadCloser: input, ctx: ctx, gate: gate, reading: reading}, nil
		},
	}}
	stat, _ := source.Stat(bg, "throttled.bin")
	slowDone := make(chan ItemResult, 1)
	go func() {
		job := Job{ParentID: 1, Generation: 1, Source: Endpoint{Kind: "LOCAL"}, Destination: Endpoint{Kind: "S3"}, ConflictPolicy: "OVERWRITE"}
		slowDone <- engine.copyItem(bg, job, Item{ID: "slow", Source: stat, DestinationKey: "throttled.bin"}, source, slowTarget)
	}()
	waitAdmission(t, entered)
	// Start the other task only once the throttled write holds its part;
	// otherwise the other task may legitimately take all four slots first.
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("throttled write never started reading")
	}
	if lent := lender.Lent(); lent != 0 {
		t.Fatal("throttled task borrowed part slots", lent)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	stat, _ = source.Stat(bg, "open.bin")
	job := Job{ParentID: 2, Generation: 1, Source: Endpoint{Kind: "LOCAL"}, Destination: Endpoint{Kind: "S3"}, ConflictPolicy: "OVERWRITE"}
	if result := engine.copyItem(ctx, job, Item{ID: "open", Source: stat, DestinationKey: "open.bin"}, source, openTarget); result.Status != ItemSucceeded {
		t.Fatalf("unthrottled task starved behind a throttled one: %+v", result)
	}
	openServer.mu.Lock()
	// The blocked throttled write holds only the one part it is reading.
	if !bytes.Equal(openServer.object, payload) || openServer.putMax > 3 {
		t.Fatalf("unthrottled write: equal=%v puts%d", bytes.Equal(openServer.object, payload), openServer.putMax)
	}
	openServer.mu.Unlock()
	close(gate)
	if result := <-slowDone; result.Status != ItemSucceeded {
		t.Fatalf("throttled copy failed: %+v", result)
	}
	slowServer.mu.Lock()
	defer slowServer.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(slowServer.object, payload) || len(throttledSizes) != 1 || throttledSizes[0] != stat.Size {
		t.Fatalf("throttled task did not stream: equal=%v reads %v", bytes.Equal(slowServer.object, payload), throttledSizes)
	}
}

func TestUnthrottledTaskBorrowsAndLimitChangesApplyToNextObject(t *testing.T) {
	source, payload := rangedSource(t, "large.bin")
	budget, _ := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	lender := idlePartLender(100)
	server, target := newLendingTarget(t, budget, lender, 30*time.Millisecond)
	var throttled atomic.Bool
	var reads atomic.Int64
	engine := &Engine{opts: Options{
		TaskThrottled: func(Job) bool { return throttled.Load() },
		DecorateReader: func(_ context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
			reads.Add(1)
			return input, nil
		},
	}}
	stat, _ := source.Stat(bg, "large.bin")
	job := Job{ParentID: 1, Generation: 1, Source: Endpoint{Kind: "LOCAL"}, Destination: Endpoint{Kind: "S3"}, ConflictPolicy: "OVERWRITE"}
	item := Item{ID: "1", Source: stat, DestinationKey: "large.bin"}
	copyOnce := func() (int64, int) {
		t.Helper()
		reads.Store(0)
		server.mu.Lock()
		server.putMax, server.object = 0, nil
		server.mu.Unlock()
		if result := engine.copyItem(bg, job, item, source, target); result.Status != ItemSucceeded {
			t.Fatalf("copy failed: %+v", result)
		}
		server.mu.Lock()
		defer server.mu.Unlock()
		if !bytes.Equal(server.object, payload) {
			t.Fatal("target differs from source")
		}
		return reads.Load(), server.putMax
	}
	throttled.Store(true)
	if reads, puts := copyOnce(); reads != 1 || puts > 4 {
		t.Fatalf("throttled object: reads%d puts%d", reads, puts)
	}
	// Limit removed while the task runs: the next object is ranged again.
	throttled.Store(false)
	if reads, puts := copyOnce(); reads != 12 || puts <= 4 {
		t.Fatalf("unthrottled object did not borrow: reads%d puts%d", reads, puts)
	}
	if lent := lender.Lent(); lent != 0 {
		t.Fatal("borrowed slots were not returned", lent)
	}
}
