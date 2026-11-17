package transferstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingProgress is a WriteProgress that checks its own invariants: the
// bytes counted in flight never go below zero or above limit, and no state is
// left more often than entered.
type recordingProgress struct {
	mu                        sync.Mutex
	limit                     int64
	pending, accepted, sent   int64
	maxPending                int64
	phases                    [PhaseUploadBuffer + 1]int
	entered                   [PhaseUploadBuffer + 1]int
	violations                []string
	onPhase                   func(WritePhase, int)
	onSending                 func(pending int64)
	acceptedAtComplete        []int64
	pendingAtComplete         []int64
	acceptedBeforePutResponse []int64
}

func (p *recordingProgress) violate(format string, args ...any) {
	p.violations = append(p.violations, fmt.Sprintf(format, args...))
}

func (p *recordingProgress) Sending(delta int64) {
	p.mu.Lock()
	p.pending += delta
	pending := p.pending
	p.maxPending = max(p.maxPending, pending)
	if pending < 0 || p.limit > 0 && pending > p.limit {
		p.violate("pending %d outside [0, %d]", pending, p.limit)
	}
	hook := p.onSending
	p.mu.Unlock()
	if hook != nil {
		hook(pending)
	}
}

func (p *recordingProgress) Sent(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n <= 0 {
		p.violate("sent %d", n)
	}
	p.sent += n
}

func (p *recordingProgress) Accepted(withdraw, confirmed int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending -= withdraw
	p.accepted += confirmed
	if p.pending < 0 {
		p.violate("pending %d after a confirmation", p.pending)
	}
}

func (p *recordingProgress) Phase(phase WritePhase, delta int) {
	p.mu.Lock()
	p.phases[phase] += delta
	if delta > 0 {
		p.entered[phase]++
	}
	if p.phases[phase] < 0 {
		p.violate("phase %d left more often than entered", phase)
	}
	hook := p.onPhase
	p.mu.Unlock()
	if hook != nil {
		hook(phase, delta)
	}
}

func (p *recordingProgress) state() (pending, accepted, sent int64, phases [PhaseUploadBuffer + 1]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending, p.accepted, p.sent, p.phases
}

func (p *recordingProgress) check(t *testing.T, accepted, sent int64) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.violations) > 0 {
		t.Fatal(p.violations)
	}
	if p.pending != 0 || p.accepted != accepted || p.sent != sent || p.phases != [PhaseUploadBuffer + 1]int{} {
		t.Fatalf("pending %d accepted %d (want %d) sent %d (want %d) phases %v", p.pending, p.accepted, accepted, p.sent, sent, p.phases)
	}
}

// meterOperation names the S3 call a request belongs to and its part number.
func meterOperation(r *http.Request) (string, int) {
	query := r.URL.Query()
	part, _ := strconv.Atoi(query.Get("partNumber"))
	switch {
	case r.Method == http.MethodPut && query.Has("uploadId"):
		return "UploadPart", part
	case r.Method == http.MethodPut:
		return "PutObject", 0
	case r.Method == http.MethodPost && query.Has("uploads"):
		return "CreateMultipartUpload", 0
	case r.Method == http.MethodPost:
		return "CompleteMultipartUpload", 0
	case r.Method == http.MethodDelete:
		return "AbortMultipartUpload", 0
	}
	return r.Method, 0
}

func s3Reply(r *http.Request, status int, body string, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	if status >= 300 {
		h.Set("Content-Type", "application/xml")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}

const slowDown = "<Error><Code>SlowDown</Code><Message>test</Message></Error>"

// meterFake answers the requests of one write without a network: each part
// (0 is a PutObject) fails its first attempts as fail asks, after reading the
// body. Complete records what the destination had confirmed by then.
type meterFake struct {
	mu       sync.Mutex
	progress *recordingProgress
	fail     map[int]int // part -> attempts answered with 503 SlowDown
	attempts map[int]int
	received atomic.Int64
	gate     func(op string, part, attempt int, r *http.Request)
}

func (f *meterFake) RoundTrip(r *http.Request) (*http.Response, error) {
	op, part := meterOperation(r)
	switch op {
	case "CreateMultipartUpload":
		return s3Reply(r, 200, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>"), nil
	case "AbortMultipartUpload":
		return s3Reply(r, 204, ""), nil
	case "CompleteMultipartUpload":
		_, _ = io.Copy(io.Discard, r.Body)
		pending, accepted, _, _ := f.progress.state()
		f.progress.mu.Lock()
		f.progress.acceptedAtComplete = append(f.progress.acceptedAtComplete, accepted)
		f.progress.pendingAtComplete = append(f.progress.pendingAtComplete, pending)
		f.progress.mu.Unlock()
		return s3Reply(r, 200, `<CompleteMultipartUploadResult><ETag>"multipart"</ETag></CompleteMultipartUploadResult>`), nil
	case "PutObject", "UploadPart":
		f.mu.Lock()
		f.attempts[part]++
		attempt := f.attempts[part]
		failing := attempt <= f.fail[part]
		f.mu.Unlock()
		if f.gate != nil {
			f.gate(op, part, attempt, r)
		}
		n, err := io.Copy(io.Discard, r.Body)
		f.received.Add(n)
		if err != nil {
			return nil, err
		}
		if failing {
			return s3Reply(r, 503, slowDown), nil
		}
		if op == "PutObject" {
			_, accepted, _, _ := f.progress.state()
			f.progress.mu.Lock()
			f.progress.acceptedBeforePutResponse = append(f.progress.acceptedBeforePutResponse, accepted)
			f.progress.mu.Unlock()
		}
		return s3Reply(r, 200, "", "ETag", fmt.Sprintf(`"part-%d"`, part)), nil
	}
	return s3Reply(r, 400, "<Error><Code>UnsupportedTestRequest</Code></Error>"), nil
}

func meterStore(t *testing.T, transport http.RoundTripper, configure ...func(*S3Config)) *S3 {
	t.Helper()
	config := S3Config{Endpoint: "https://storage.invalid", Bucket: "bucket", AccessKey: "access", SecretKey: "secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 2, RetryMaxAttempts: 3, HTTPClient: &http.Client{Transport: transport}}
	for _, apply := range configure {
		apply(&config)
	}
	store, err := NewS3(config)
	if err != nil {
		t.Fatal(err)
	}
	store.retryer.random = func() float64 { return 0 }
	return store
}

func payload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	return data
}

func rangesOf(data []byte) RangeFunc {
	return func(_ context.Context, offset, length int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
	}
}

// A failed attempt's bytes are withdrawn before the next attempt sends them
// again: the object never counts them twice, while the speed counts both.
func TestRequestMeterWithdrawsFailedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		ranged bool
		fail   map[int]int
	}{
		{"PutObject", 3 << 20, false, map[int]int{0: 1}},
		{"UploadPart", int(2*minPartSize) + 1<<20, false, map[int]int{2: 1}},
		{"RangedUploadPart", int(2*minPartSize) + 1<<20, true, map[int]int{1: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := payload(tc.size)
			progress := &recordingProgress{limit: int64(tc.size)}
			fake := &meterFake{progress: progress, fail: tc.fail, attempts: map[int]int{}}
			fake.gate = func(_ string, part, attempt int, _ *http.Request) {
				// One request at a time: every attempt starts with nothing
				// in flight, the failed attempt's bytes withdrawn.
				pending, _, _, phases := progress.state()
				if pending != 0 || phases != [PhaseUploadBuffer + 1]int{PhaseRequestLimit: 1} {
					t.Errorf("part %d attempt %d began with %d bytes pending, states %v", part, attempt, pending, phases)
				}
			}
			store := meterStore(t, fake, func(c *S3Config) {
				c.PartConcurrency = 1
				if tc.ranged {
					memory, _ := NewUploadMemoryBudget(64 << 20)
					c.UploadMemory, c.RangedReadMinSize = memory, 1
				}
			})
			options := WriteOptions{Size: int64(tc.size), DestinationAbsent: true, Overwrite: true, Progress: progress}
			if tc.ranged {
				options.Ranges = rangesOf(data)
			}
			result, err := store.Write(context.Background(), "object", bytes.NewReader(data), options)
			if err != nil || !result.Committed {
				t.Fatal(result, err)
			}
			extra := int64(0)
			for part, failures := range tc.fail {
				length := int64(tc.size)
				if part > 0 {
					length = min(minPartSize, int64(tc.size)-int64(part-1)*minPartSize)
				}
				extra += int64(failures) * length
			}
			progress.check(t, int64(tc.size), int64(tc.size)+extra)
			if got := fake.received.Load(); got != int64(tc.size)+extra {
				t.Fatalf("server received %d, sent counted %d", got, int64(tc.size)+extra)
			}
		})
	}
}

// After an attempt failed, its body may still be read, for example by
// net/http finishing a body the server rejected, until the SDK closes it.
// Those reads count as sent, never as the object's progress: not while the
// request waits for its retry, not after the next attempt began, and not
// after the call ended.
func TestRequestMeterIgnoresReadsOfAFailedAttempt(t *testing.T) {
	progress := &recordingProgress{limit: 300}
	_, meter := meterRequest(context.Background(), progress)
	first := meter.begin(true)
	meter.read(first, 100)
	meter.failed()
	meter.read(first, 50)
	if pending, _, _, phases := progress.state(); pending != 0 || phases != [PhaseUploadBuffer + 1]int{PhaseRetryWait: 1} {
		t.Fatalf("after the failure: pending %d, states %v", pending, phases)
	}
	second := meter.begin(false)
	meter.read(first, 50)
	meter.read(second, 300)
	if pending, _, _, phases := progress.state(); pending != 300 || phases != [PhaseUploadBuffer + 1]int{PhaseSending: 1} {
		t.Fatalf("second attempt: pending %d, states %v", pending, phases)
	}
	meter.finish(true, 300)
	meter.read(second, 10)
	progress.check(t, 300, 100+50+50+300+10)
}

// net/http sends a request again with a body from GetBody when a reused
// connection fails. The old body may still be read after the new one began:
// those reads count as sent, never as the object's progress.
func TestRequestMeterIgnoresLateReads(t *testing.T) {
	size := 3 << 20
	data := payload(size)
	progress := &recordingProgress{limit: int64(size)}
	transport := replayRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.GetBody == nil {
			t.Fatal("upload without a replayable body")
		}
		first := make([]byte, size/3)
		if _, err := io.ReadFull(r.Body, first); err != nil {
			return nil, err
		}
		replay, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		again, err := io.ReadAll(replay)
		if err != nil || !bytes.Equal(again, data) {
			t.Fatalf("replay read %d bytes: %v", len(again), err)
		}
		// The old connection's writer reads on.
		late, err := io.ReadAll(r.Body)
		if err != nil || len(late) != size-size/3 {
			t.Fatalf("late read %d bytes: %v", len(late), err)
		}
		if pending, _, _, _ := progress.state(); pending != int64(size) {
			t.Fatalf("pending %d after the replay, want %d", pending, size)
		}
		return s3Reply(r, 200, "", "ETag", `"etag"`), nil
	})
	store := meterStore(t, transport)
	if _, err := store.Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}); err != nil {
		t.Fatal(err)
	}
	progress.check(t, int64(size), 2*int64(size))
}

// The SDK reads a body before signing it for an http:// endpoint. Only the
// bytes that reach the destination count as sent.
func TestRequestMeterSkipsSigV4PreRead(t *testing.T) {
	size := 3 << 20
	var received atomic.Int64
	var hashed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hashed.Store(r.Header.Get("X-Amz-Content-Sha256") != "" && r.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD")
		n, _ := io.Copy(io.Discard, r.Body)
		received.Add(n)
		w.Header().Set("ETag", `"etag"`)
	}))
	defer server.Close()
	progress := &recordingProgress{limit: int64(size)}
	store := meterStore(t, server.Client().Transport, func(c *S3Config) { c.Endpoint = server.URL })
	if _, err := store.Write(context.Background(), "object", bytes.NewReader(payload(size)), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}); err != nil {
		t.Fatal(err)
	}
	if !hashed.Load() {
		t.Fatal("the request was not signed with its payload hash")
	}
	progress.check(t, int64(size), received.Load())
	if received.Load() != int64(size) {
		t.Fatalf("server received %d bytes", received.Load())
	}
}

// The SDK's requests carry no HTTP version, so net/http's HTTP/1.1 client
// does not wait for 100 Continue and sends the body at once. A request the
// server rejects without reading it withdraws what it counted; the bytes
// written count as sent only.
func TestRequestMeterExpectContinueRejected(t *testing.T) {
	size := 3 << 20
	var expected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected.Store(strings.EqualFold(r.Header.Get("Expect"), "100-continue"))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, slowDown)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.ExpectContinueTimeout = time.Hour
	progress := &recordingProgress{limit: int64(size)}
	store := meterStore(t, transport, func(c *S3Config) {
		c.Endpoint, c.RetryMaxAttempts, c.ThrottleRetryMaxAttempts = server.URL, 1, 1
	})
	if _, err := store.Write(context.Background(), "object", bytes.NewReader(payload(size)), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}); err == nil {
		t.Fatal("rejected upload succeeded")
	}
	if !expected.Load() {
		t.Fatal("the upload did not ask for 100 Continue")
	}
	pending, accepted, sent, phases := progress.state()
	if pending != 0 || accepted != 0 || phases != [PhaseUploadBuffer + 1]int{} || sent > int64(size) || len(progress.violations) > 0 {
		t.Fatalf("pending %d accepted %d sent %d states %v: %v", pending, accepted, sent, phases, progress.violations)
	}
}

func awaitSignal(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(time.Minute): // hang guard only
		t.Fatalf("the write never reached %s", what)
	}
}

// Each wait of a write shows as its own state: a request limit holding the
// request, the backoff after a failed attempt, and parts waiting for upload
// memory.
func TestRequestPhases(t *testing.T) {
	size := 3 << 20
	data := payload(size)
	t.Run("RequestLimit", func(t *testing.T) {
		progress := &recordingProgress{limit: int64(size)}
		holding, release := make(chan struct{}), make(chan struct{})
		transport := replayRoundTripper(func(r *http.Request) (*http.Response, error) {
			// A request limiter lets the request go.
			close(holding)
			<-release
			RequestAdmitted(r.Context())
			if _, _, _, phases := progress.state(); phases[PhaseRequestLimit] != 0 || phases[PhaseSending] != 1 {
				t.Errorf("admitted request states %v", phases)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			return s3Reply(r, 200, "", "ETag", `"etag"`), nil
		})
		done := make(chan error, 1)
		go func() {
			_, err := meterStore(t, transport).Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress})
			done <- err
		}()
		awaitSignal(t, holding, "the request limit")
		if _, _, _, phases := progress.state(); phases != [PhaseUploadBuffer + 1]int{PhaseRequestLimit: 1} {
			t.Fatalf("held request states %v", phases)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		progress.check(t, int64(size), int64(size))
	})
	t.Run("RetryWait", func(t *testing.T) {
		progress := &recordingProgress{limit: int64(size)}
		fake := &meterFake{progress: progress, fail: map[int]int{0: 1}, attempts: map[int]int{}}
		store := meterStore(t, fake)
		waiting, release := make(chan struct{}), make(chan struct{})
		store.retryer.random = func() float64 { close(waiting); <-release; return 0 }
		done := make(chan error, 1)
		go func() {
			_, err := store.Write(context.Background(), "object", bytes.NewReader(data), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress})
			done <- err
		}()
		awaitSignal(t, waiting, "its retry backoff")
		if pending, _, _, phases := progress.state(); pending != 0 || phases != [PhaseUploadBuffer + 1]int{PhaseRetryWait: 1} {
			t.Fatalf("backoff: pending %d, states %v", pending, phases)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		progress.check(t, int64(size), 2*int64(size))
	})
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprintf("UploadBuffer/ranged=%v", ranged), func(t *testing.T) {
			size := int(2*minPartSize) + 1<<20
			data := payload(size)
			memory, _ := NewUploadMemoryBudget(minPartSize)
			held, err := memory.acquire(context.Background(), minPartSize)
			if err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			var once sync.Once
			progress := &recordingProgress{limit: int64(size), onPhase: func(phase WritePhase, delta int) {
				if phase == PhaseUploadBuffer && delta > 0 {
					once.Do(func() { close(waiting) })
				}
			}}
			fake := &meterFake{progress: progress, attempts: map[int]int{}}
			store := meterStore(t, fake, func(c *S3Config) {
				c.UploadMemory = memory
				if ranged {
					c.RangedReadMinSize = 1
				}
			})
			options := WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}
			if ranged {
				options.Ranges = rangesOf(data)
			}
			done := make(chan error, 1)
			go func() {
				_, err := store.Write(context.Background(), "object", bytes.NewReader(data), options)
				done <- err
			}()
			awaitSignal(t, waiting, "the upload memory budget")
			if pending, _, _, phases := progress.state(); pending != 0 || phases != [PhaseUploadBuffer + 1]int{PhaseUploadBuffer: 1} {
				t.Fatalf("memory wait: pending %d, states %v", pending, phases)
			}
			held()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			progress.check(t, int64(size), int64(size))
		})
	}
}

// A PutObject confirms its bytes only with the response: a body sent in
// full is still in flight until then.
func TestPutObjectNeverFinalizesBeforeResponse(t *testing.T) {
	size := 3 << 20
	progress := &recordingProgress{limit: int64(size)}
	fake := &meterFake{progress: progress, attempts: map[int]int{}}
	if _, err := meterStore(t, fake).Write(context.Background(), "object", bytes.NewReader(payload(size)), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}); err != nil {
		t.Fatal(err)
	}
	if got := progress.acceptedBeforePutResponse; len(got) != 1 || got[0] != 0 || progress.maxPending != int64(size) {
		t.Fatalf("confirmed before the response: %v, at most %d pending", got, progress.maxPending)
	}
	progress.check(t, int64(size), int64(size))
}

// Every part is confirmed before CompleteMultipartUpload is sent, so the
// object finalizes only while the upload is being completed.
func TestMultipartFinalizesOnlyDuringComplete(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprintf("ranged=%v", ranged), func(t *testing.T) {
			size := int(3*minPartSize) + 1<<20
			data := payload(size)
			progress := &recordingProgress{limit: int64(size)}
			fake := &meterFake{progress: progress, attempts: map[int]int{}}
			var confirmedBeforeLast atomic.Int64
			fake.gate = func(_ string, part, _ int, _ *http.Request) {
				if _, accepted, _, _ := progress.state(); accepted >= int64(size) {
					t.Errorf("part %d sent after every byte was confirmed", part)
				} else {
					confirmedBeforeLast.Store(accepted)
				}
			}
			store := meterStore(t, fake, func(c *S3Config) {
				if ranged {
					memory, _ := NewUploadMemoryBudget(64 << 20)
					c.UploadMemory, c.RangedReadMinSize = memory, 1
				}
			})
			options := WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true, Progress: progress}
			if ranged {
				options.Ranges = rangesOf(data)
			}
			if _, err := store.Write(context.Background(), "object", bytes.NewReader(data), options); err != nil {
				t.Fatal(err)
			}
			if got := progress.acceptedAtComplete; len(got) != 1 || got[0] != int64(size) || progress.pendingAtComplete[0] != 0 {
				t.Fatalf("Complete began with %v confirmed and %v pending of %d", got, progress.pendingAtComplete, size)
			}
			if progress.entered[PhaseUploadBuffer] != 0 || progress.entered[PhaseRetryWait] != 0 {
				t.Fatalf("a write with free memory and no failure waited: %v", progress.entered)
			}
			progress.check(t, int64(size), int64(size))
		})
	}
}

// A write without a progress counter is not metered.
func TestRequestMeterOffWithoutProgress(t *testing.T) {
	size := 3 << 20
	transport := replayRoundTripper(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Body.(*meteredBody); ok || requestMeterFrom(r.Context()) != nil {
			t.Error("an unmetered write carried a meter")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return s3Reply(r, 200, "", "ETag", `"etag"`), nil
	})
	if _, err := meterStore(t, transport).Write(context.Background(), "object", bytes.NewReader(payload(size)), WriteOptions{Size: int64(size), DestinationAbsent: true, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
}
