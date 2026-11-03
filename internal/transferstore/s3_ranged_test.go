package transferstore

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
	"strings"
	"sync"
	"testing"
	"time"
)

// rangeS3 serves one source object with conditional byte ranges and accepts a
// multipart target, counting concurrent requests. latency is added to every
// object GET and UploadPart; connectionRate caps each response or part body.
type rangeS3 struct {
	mu                 sync.Mutex
	data               []byte
	etag               string
	parts              map[int][]byte
	target             []byte
	completed, aborted int
	gets, ranged, puts int
	getActive, getMax  int
	putActive, putMax  int
	latency            time.Duration
	connectionRate     int64
	ignoreRange        bool
	truncate           map[int64]int // range offset -> truncated responses left
	onRange            func(offset int64)
	onPart             func(ctx context.Context, number int)
	server             *httptest.Server
	budget             *UploadMemoryBudget
	peakMemory         int64
}

func newRangeS3(t *testing.T, data []byte) *rangeS3 {
	t.Helper()
	r := &rangeS3{data: data, etag: objectETag(data), parts: map[int][]byte{}, truncate: map[int64]int{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *rangeS3) store(t *testing.T, budget *UploadMemoryBudget, lender *PartLender, spool string) *S3 {
	t.Helper()
	r.budget = budget
	s, err := NewS3(S3Config{Endpoint: r.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 4, UploadMemory: budget, PartLender: lender, RangedReadMinSize: 1, SpoolDirectory: spool, RetryMaxAttempts: 2, HTTPClient: r.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// throttled copies at connectionRate, modelling one TCP stream's ceiling.
func (r *rangeS3) throttled(w io.Writer, input io.Reader) error {
	if r.connectionRate <= 0 {
		_, err := io.Copy(w, input)
		return err
	}
	chunk := make([]byte, 256<<10)
	start := time.Now()
	var sent int64
	for {
		n, err := input.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return werr
			}
			sent += int64(n)
			if due := time.Duration(sent * int64(time.Second) / r.connectionRate); due > time.Since(start) {
				time.Sleep(due - time.Since(start))
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r *rangeS3) serve(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	key := strings.TrimPrefix(req.URL.Path, "/bucket/")
	switch {
	case req.Method == "GET" && key == "source":
		r.serveSource(w, req)
	case req.Method == "POST" && q.Has("uploads"):
		r.mu.Lock()
		r.parts = map[int][]byte{}
		r.mu.Unlock()
		fmt.Fprint(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>target</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>")
	case req.Method == "PUT" && q.Get("uploadId") != "":
		number, _ := strconv.Atoi(q.Get("partNumber"))
		r.mu.Lock()
		r.puts++
		r.putActive++
		r.putMax = max(r.putMax, r.putActive)
		if r.budget != nil {
			used, _ := memoryState(r.budget)
			r.peakMemory = max(r.peakMemory, used)
		}
		r.mu.Unlock()
		defer func() { r.mu.Lock(); r.putActive--; r.mu.Unlock() }()
		// Read first: the server only notices a cancelled client after the body.
		var body bytes.Buffer
		if err := r.throttled(&body, req.Body); err != nil {
			return
		}
		if r.onPart != nil {
			r.onPart(req.Context(), number)
		}
		time.Sleep(r.latency)
		r.mu.Lock()
		r.parts[number] = body.Bytes()
		r.mu.Unlock()
		w.Header().Set("ETag", objectETag(body.Bytes()))
	case req.Method == "POST" && q.Get("uploadId") != "":
		var complete struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(req.Body).Decode(&complete); err != nil {
			protocolError(w, 400, "InvalidRequest")
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		var result []byte
		for index, part := range complete.Parts {
			if part.PartNumber != index+1 || part.ETag != objectETag(r.parts[part.PartNumber]) {
				protocolError(w, 400, "InvalidPart")
				return
			}
			result = append(result, r.parts[part.PartNumber]...)
		}
		r.target = result
		r.completed++
		fmt.Fprint(w, "<CompleteMultipartUploadResult><ETag>\"multipart\"</ETag></CompleteMultipartUploadResult>")
	case req.Method == "DELETE" && q.Get("uploadId") != "":
		r.mu.Lock()
		r.aborted++
		r.parts = map[int][]byte{}
		r.mu.Unlock()
		w.WriteHeader(204)
	case req.Method == "GET" && q.Get("uploadId") != "":
		protocolError(w, 404, "NoSuchUpload")
	default:
		protocolError(w, 400, "UnsupportedTestRequest")
	}
}

func (r *rangeS3) serveSource(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.gets++
	r.getActive++
	r.getMax = max(r.getMax, r.getActive)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.getActive--; r.mu.Unlock() }()
	time.Sleep(r.latency)
	start, end := int64(0), int64(-1)
	if value := req.Header.Get("Range"); value != "" && !r.ignoreRange {
		first, last, _ := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
		start, _ = strconv.ParseInt(first, 10, 64)
		end, _ = strconv.ParseInt(last, 10, 64)
		if r.onRange != nil && end > start {
			r.onRange(start)
		}
	}
	r.mu.Lock()
	data, etag := r.data, r.etag
	truncated := false
	if end > start && r.truncate[start] > 0 {
		r.truncate[start]--
		truncated = true
	}
	if end >= 0 {
		r.ranged++
	}
	r.mu.Unlock()
	if match := req.Header.Get("If-Match"); match != "" && match != etag {
		protocolError(w, 412, "PreconditionFailed")
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/x-test")
	w.Header().Set("Last-Modified", protocolTime.Format(http.TimeFormat))
	body := data
	status := 200
	if end >= 0 {
		body = data[start : end+1]
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if truncated {
		// The server closes a connection whose handler wrote too little.
		_, _ = w.Write(body[:len(body)/2])
		return
	}
	_ = r.throttled(w, bytes.NewReader(body))
}

func rangedPayload(parts int) []byte {
	payload := make([]byte, int64(parts)*minPartSize-12345)
	for i := range payload {
		payload[i] = byte(i*7 + i/4099)
	}
	return payload
}

func idleLender(idle int) *PartLender {
	lender := NewPartLender()
	lender.SetIdle(func() int { return idle })
	return lender
}

func mustBudget(t *testing.T, bytes int64) *UploadMemoryBudget {
	t.Helper()
	budget, err := NewUploadMemoryBudget(bytes)
	if err != nil {
		t.Fatal(err)
	}
	return budget
}

func rangedWrite(ctx context.Context, s *S3, size int64) (WriteResult, error) {
	ranges, object, err := s.OpenRanges(ctx, "source", OpenOptions{})
	if err != nil {
		return WriteResult{}, err
	}
	defer ranges.Close()
	if object.Size != size || object.ContentType != "application/x-test" {
		return WriteResult{}, fmt.Errorf("probe object %+v", object)
	}
	return s.Write(ctx, "target", strings.NewReader("unused"), WriteOptions{Size: size, Overwrite: true, Ranges: ranges.OpenRange})
}

func TestRangedMultipartBorrowsIdleSlotsAndKeepsBaseWhenBusy(t *testing.T) {
	payload := rangedPayload(12)
	for _, tc := range []struct {
		name string
		idle int
	}{{"idle", 100}, {"busy", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			server := newRangeS3(t, payload)
			server.latency = 40 * time.Millisecond
			lender := idleLender(tc.idle)
			s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), lender, t.TempDir())
			result, err := rangedWrite(context.Background(), s, int64(len(payload)))
			if err != nil || !result.Committed {
				t.Fatalf("ranged write %+v %v", result, err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if !bytes.Equal(server.target, payload) || server.completed != 1 || server.aborted != 0 || server.puts != 12 {
				t.Fatalf("target integrity: completed%d aborted%d puts%d", server.completed, server.aborted, server.puts)
			}
			// One probe plus one conditional range per part; no whole-object GET.
			if server.gets != 13 || server.ranged != 13 {
				t.Fatalf("source requests gets%d ranged%d", server.gets, server.ranged)
			}
			if tc.idle > 0 && (server.putMax <= 4 || server.getMax <= 4) {
				t.Fatalf("idle node did not lend slots: puts%d gets%d", server.putMax, server.getMax)
			}
			if tc.idle == 0 && (server.putMax != 4 || server.getMax > 4) {
				t.Fatalf("busy node changed base concurrency: puts%d gets%d", server.putMax, server.getMax)
			}
			if lender.Lent() != 0 {
				t.Fatal("borrowed slots were not returned", lender.Lent())
			}
		})
	}
}

func TestRangedPartsStayResidentWithinNodeBudget(t *testing.T) {
	payload := rangedPayload(12)
	server := newRangeS3(t, payload)
	server.latency = 40 * time.Millisecond
	budget := mustBudget(t, 8*minPartSize)
	spool := t.TempDir()
	s := server.store(t, budget, idleLender(100), spool)
	// A tiny per-write buffer would make the streaming path spool every part.
	s.maxBufferedBytes = 1 << 20
	if result, err := rangedWrite(context.Background(), s, int64(len(payload))); err != nil || !result.Committed {
		t.Fatalf("ranged write %+v %v", result, err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !bytes.Equal(server.target, payload) {
		t.Fatal("target differs")
	}
	// Borrowing stops while it would eat into the quarter kept for FIFO waiters:
	// four base parts plus two borrowed fit within six of the eight parts.
	if server.peakMemory > 8*minPartSize || server.putMax > 6 || server.putMax <= 4 {
		t.Fatalf("resident %d puts%d", server.peakMemory, server.putMax)
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 {
		t.Fatalf("budget leaked used%d waiting%d", used, waiting)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatal("ranged parts were spooled to disk", len(entries))
	}
}

func TestBorrowedMemoryNeverOvertakesQueuedParts(t *testing.T) {
	budget := mustBudget(t, 4)
	hold, err := budget.acquire(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := budget.tryAcquire(1, 1); ok {
		t.Fatal("borrow consumed the reserve")
	}
	if release, ok := budget.tryAcquire(1, 0); !ok {
		t.Fatal("free memory was not lent")
	} else {
		release()
	}
	queued := make(chan func(), 1)
	go func() {
		release, _ := budget.acquire(context.Background(), 2)
		queued <- release
	}()
	for _, waiting := memoryState(budget); waiting == 0; _, waiting = memoryState(budget) {
		time.Sleep(time.Millisecond)
	}
	if _, ok := budget.tryAcquire(1, 0); ok {
		t.Fatal("borrow overtook a queued part")
	}
	hold()
	(<-queued)()
	if used, _ := memoryState(budget); used != 0 {
		t.Fatal("budget leaked", used)
	}
}

func TestRangedSourceChangeFailsAndAborts(t *testing.T) {
	payload := rangedPayload(8)
	server := newRangeS3(t, payload)
	server.onRange = func(offset int64) {
		if offset == 3*minPartSize {
			server.mu.Lock()
			server.etag = "\"replaced\""
			server.mu.Unlock()
		}
	}
	budget := mustBudget(t, DefaultUploadMemoryBytes)
	lender := idleLender(100)
	s := server.store(t, budget, lender, t.TempDir())
	if _, _, err := s.OpenRanges(context.Background(), "source", OpenOptions{Expected: &Object{Key: "source", Size: int64(len(payload)), ETag: "stale"}}); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("stale expectation was accepted", err)
	}
	result, err := rangedWrite(context.Background(), s, int64(len(payload)))
	if !errors.Is(err, ErrSourceChanged) || result.Committed {
		t.Fatalf("changed source accepted %+v %v", result, err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.completed != 0 || server.aborted != 1 {
		t.Fatalf("completed%d aborted%d", server.completed, server.aborted)
	}
	if used, _ := memoryState(budget); used != 0 || lender.Lent() != 0 {
		t.Fatalf("leaked memory%d lent%d", used, lender.Lent())
	}
}

func TestRangedCancelAbortsAndReturnsBorrowedSlots(t *testing.T) {
	payload := rangedPayload(12)
	server := newRangeS3(t, payload)
	arrived := make(chan struct{}, 16)
	server.onPart = func(ctx context.Context, _ int) { arrived <- struct{}{}; <-ctx.Done() }
	budget := mustBudget(t, DefaultUploadMemoryBytes)
	lender := idleLender(100)
	s := server.store(t, budget, lender, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := rangedWrite(ctx, s, int64(len(payload)))
		done <- err
	}()
	for range 6 {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("borrowed parts did not start")
		}
	}
	if lender.Lent() == 0 {
		t.Fatal("no slots were borrowed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the ranged upload")
	}
	server.mu.Lock()
	aborted, completed := server.aborted, server.completed
	server.mu.Unlock()
	if aborted != 1 || completed != 0 || lender.Lent() != 0 {
		t.Fatalf("aborted%d completed%d lent%d", aborted, completed, lender.Lent())
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 {
		t.Fatalf("budget leaked used%d waiting%d", used, waiting)
	}
}

func TestRangedReadRetriesTruncatedBodyOnly(t *testing.T) {
	payload := rangedPayload(6)
	server := newRangeS3(t, payload)
	server.truncate[0] = 1
	server.truncate[2*minPartSize] = 2
	s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), idleLender(0), t.TempDir())
	if result, err := rangedWrite(context.Background(), s, int64(len(payload))); err != nil || !result.Committed {
		t.Fatalf("retried ranges %+v %v", result, err)
	}
	server.mu.Lock()
	if !bytes.Equal(server.target, payload) || server.ranged != 1+6+3 {
		t.Fatalf("ranged%d", server.ranged)
	}
	server.truncate[minPartSize] = rangeReadAttempts
	server.mu.Unlock()
	if _, err := rangedWrite(context.Background(), s, int64(len(payload))); err == nil {
		t.Fatal("persistent truncation succeeded")
	}
}

func TestRangedOpenFallsBackWhenProviderIgnoresRange(t *testing.T) {
	server := newRangeS3(t, rangedPayload(2))
	server.ignoreRange = true
	s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), nil, "")
	if _, _, err := s.OpenRanges(context.Background(), "source", OpenOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("ignored Range was not reported as unsupported", err)
	}
}

func TestRangedWriteEligibility(t *testing.T) {
	server := newRangeS3(t, nil)
	s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), nil, "")
	s.rangedMinSize = defaultRangedMinSize
	for _, tc := range []struct {
		size int64
		want bool
		part int64
	}{{defaultRangedMinSize - 1, false, 0}, {defaultRangedMinSize, true, minPartSize}, {largeObjectSize, true, largeObjectPartSize}, {600 << 30, true, 600 << 30 / 10000}, {700 << 30, false, 0}} {
		if got := s.RangedWrite(tc.size); got != tc.want {
			t.Fatalf("RangedWrite(%d) = %v", tc.size, got)
		}
		if tc.want && s.multipartPartSize(tc.size, true) < tc.part {
			t.Fatalf("part size for %d is %d", tc.size, s.multipartPartSize(tc.size, true))
		}
	}
	if s.multipartPartSize(largeObjectSize, false) != minPartSize {
		t.Fatal("streamed parts grew and would spool")
	}
	s.uploadMemory = nil
	if s.RangedWrite(largeObjectSize) {
		t.Fatal("ranged parts without a node memory budget")
	}
}

func TestLocalRangedSourceToS3(t *testing.T) {
	payload := rangedPayload(6)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	expected, err := local.Stat(context.Background(), "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	server := newRangeS3(t, nil)
	s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), idleLender(100), t.TempDir())
	write := func(change func(int64)) error {
		ranges, object, err := local.OpenRanges(context.Background(), "large.bin", OpenOptions{Expected: &expected})
		if err != nil {
			return err
		}
		defer ranges.Close()
		read := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			if change != nil {
				change(offset)
			}
			return ranges.OpenRange(ctx, offset, length)
		}
		_, err = s.Write(context.Background(), "target", strings.NewReader("unused"), WriteOptions{Size: object.Size, Overwrite: true, Ranges: read})
		return err
	}
	if err := write(nil); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	if !bytes.Equal(server.target, payload) || server.completed != 1 {
		t.Fatal("local ranged copy differs")
	}
	server.mu.Unlock()
	var once sync.Once
	err = write(func(offset int64) {
		if offset == 4*minPartSize {
			once.Do(func() {
				later := expected.ModifiedAt.Add(time.Minute)
				_ = os.Chtimes(filepath.Join(root, "large.bin"), later, later)
			})
		}
	})
	server.mu.Lock()
	defer server.mu.Unlock()
	if !errors.Is(err, ErrSourceChanged) || server.completed != 1 || server.aborted != 1 {
		t.Fatalf("local change mid-copy: %v completed%d aborted%d", err, server.completed, server.aborted)
	}
}

// The old path streams one GET into four part slots; the ranged path gives
// each slot its own GET. Each connection is capped and every request pays a
// round trip, as on the test cluster's HTTP/1.1 endpoint.
func TestRangedThroughputGainWithRequestLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison")
	}
	payload := rangedPayload(16)
	size := int64(len(payload))
	run := func(ranged bool) time.Duration {
		server := newRangeS3(t, payload)
		server.latency = 20 * time.Millisecond
		server.connectionRate = 64 << 20
		s := server.store(t, mustBudget(t, DefaultUploadMemoryBytes), idleLender(100), t.TempDir())
		start := time.Now()
		var err error
		if ranged {
			_, err = rangedWrite(context.Background(), s, size)
		} else {
			var input io.ReadCloser
			input, _, err = s.Open(context.Background(), "source", OpenOptions{})
			if err == nil {
				_, err = s.Write(context.Background(), "target", input, WriteOptions{Size: size, Overwrite: true})
				input.Close()
			}
		}
		elapsed := time.Since(start)
		server.mu.Lock()
		defer server.mu.Unlock()
		if err != nil || !bytes.Equal(server.target, payload) {
			t.Fatalf("ranged=%v: %v", ranged, err)
		}
		return elapsed
	}
	streamed, ranged := run(false), run(true)
	rate := func(d time.Duration) float64 { return float64(size) / (1 << 20) / d.Seconds() }
	t.Logf("%d MiB object, 20 ms per request, 64 MiB/s per connection: streamed %v (%.0f MiB/s), ranged %v (%.0f MiB/s), %.1fx",
		size>>20, streamed.Round(time.Millisecond), rate(streamed), ranged.Round(time.Millisecond), rate(ranged), streamed.Seconds()/ranged.Seconds())
	if ranged*2 > streamed {
		t.Fatalf("ranged path was not at least twice as fast: %v vs %v", ranged, streamed)
	}
}
