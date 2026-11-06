package nodecontrol

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// fakeClock drives the throttle window and the measured rates. Rate-limit
// waits still use real time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func useClock(c *Controller, start time.Time) *fakeClock {
	clock := &fakeClock{now: start}
	c.mu.Lock()
	c.now = clock.Now
	c.mu.Unlock()
	return clock
}

func autoAdjust(t *testing.T, c *Controller) map[string]any {
	t.Helper()
	result, err := c.Snapshot()
	must(t, err)
	return result["autoAdjust"].(map[string]any)
}

// respond reports responses from 64 concurrent requests; every tenth is
// throttled. Successes interleave with the throttling responses, which reset
// the removed consecutive-503 counter long before it reached its threshold.
func respond(t *testing.T, c *Controller, operation string, count int) {
	t.Helper()
	var group sync.WaitGroup
	for worker := range 64 {
		group.Go(func() {
			for i := worker; i < count; i += 64 {
				request := RequestContext{Operation: operation, StatusCode: 200}
				if i%10 == 0 {
					request.StatusCode, request.ErrorCode = 503, "SlowDown"
				}
				if err := c.ObserveResponse(request); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	group.Wait()
}

func TestInterleavedThrottlingLowersItsOwnLimitOncePerWindowAndRecovers(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 500, PutQPS: 1000, HeadQPS: 1000, ReduceFactor: 0.9, IncreaseFactor: 1.2, MinQPS: 10, RecoveryInterval: time.Hour})
	clock := useClock(f.controller, time.Now())
	respond(t, f.controller, "PutObject", 2000)
	history, err := f.controller.History(10)
	must(t, err)
	if len(history) != 1 || history[0].Type != "put" || history[0].ReasonCode != "OSS_503" || history[0].Value != 900 || history[0].ThrottleCount < 5 {
		t.Fatalf("200 interleaved throttling responses must lower PUT exactly once in one window: %+v", history)
	}
	if rate(t, f.controller, "head")["limitQps"] != float64(1000) || rate(t, f.controller, "list")["limitQps"] != float64(500) {
		t.Fatal("throttled PUTs lowered other limits")
	}

	// Object GETs have no node limit: counted, but nothing is lowered.
	respond(t, f.controller, "GetObject", 1000)
	// Sporadic throttling, 0.5% of a busy limit's responses, is not sustained.
	for range 995 {
		must(t, f.controller.ObserveResponse(RequestContext{Operation: "HeadObject", StatusCode: 200}))
	}
	for range 5 {
		must(t, f.controller.ObserveResponse(RequestContext{Operation: "HeadObject", StatusCode: 503}))
	}
	if history, _ = f.controller.History(10); len(history) != 1 || rate(t, f.controller, "head")["limitQps"] != float64(1000) {
		t.Fatalf("GET or sporadic HEAD throttling lowered a limit: %+v", history)
	}
	auto := autoAdjust(t, f.controller)
	if auto["throttleThreshold"] != 5 || auto["throttleRatio"] != 0.01 || auto["throttleWindowMs"] != int64(5000) || auto["throttleCount"].(int64) < 100 || auto["lastThrottleAt"] == nil || auto["nextRecoveryAt"] == nil {
		t.Fatalf("snapshot does not explain the throttle window: %v", auto)
	}
	if _, old := auto["consecutive503Count"]; old {
		t.Fatal("snapshot still reports the consecutive rule")
	}

	// Throttling that continues past the window lowers the limit once more.
	clock.Advance(5 * time.Second)
	respond(t, f.controller, "PutObject", 2000)
	if got := rate(t, f.controller, "put")["limitQps"]; got != float64(810) {
		t.Fatalf("continued throttling after one window: PUT limit %v, want 810", got)
	}
	must(t, f.controller.Tick(clock.Now()))
	if rate(t, f.controller, "put")["limitQps"] != float64(810) {
		t.Fatal("recovered while throttling was still sustained")
	}
	must(t, f.controller.Tick(clock.Now().Add(2*time.Hour)))
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(972) || got["state"] != "RECOVERING" {
		t.Fatalf("wrong recovery step: %v", got)
	}
	must(t, f.controller.Tick(clock.Now().Add(4*time.Hour)))
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(1000) || got["state"] != "RECOVERED" {
		t.Fatalf("did not recover to target: %v", got)
	}
}

func TestThrottlingLowersFromTheMeasuredRate(t *testing.T) {
	f := newFixture(t, Options{PutQPS: 1000, MinQPS: 10, RecoveryInterval: time.Hour})
	clock := useClock(f.controller, time.Now().Truncate(time.Second))
	// The node sent at most 300 PUTs per second, far below its limit of 1000.
	f.controller.mu.Lock()
	for _, rate := range []int64{200, 300, 250} {
		clock.Advance(time.Second)
		f.controller.addSample(kindIndex("put"), rate)
	}
	f.controller.mu.Unlock()
	clock.Advance(time.Second)
	for range 5 {
		must(t, f.controller.ObserveResponse(RequestContext{Operation: "UploadPart", StatusCode: 503, ErrorCode: "SlowDown"}))
	}
	history, err := f.controller.History(1)
	must(t, err)
	if len(history) != 1 || history[0].MeasuredQPS != 300 || history[0].PreviousValue != 1000 || history[0].Value != 270 {
		t.Fatalf("reduction must start from the measured 300/s: %+v", history)
	}
}

func TestThrottlingResponsesAreClassifiedPerOperationOverHTTP(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 1000, PutQPS: 1000, HeadQPS: 1000, Threshold: 1, MinQPS: 10, RecoveryInterval: time.Hour})
	useClock(f.controller, time.Now())
	const limited = `<Error><Code>RequestLimitExceeded</Code><Message>slow</Message></Error>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Request-Id", "trace-"+r.Method)
		switch {
		case r.Method == "GET" && r.URL.Query().Has("list-type"):
			w.WriteHeader(429)
			io.WriteString(w, `<Error><Code>TooManyRequests</Code></Error>`)
		case r.Method == "GET":
			w.WriteHeader(503)
			io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
		case r.Method == "PUT":
			w.WriteHeader(400)
			io.WriteString(w, limited)
		case r.Method == "HEAD":
			w.WriteHeader(429)
		default:
			w.WriteHeader(500)
			io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: f.controller.Transport(nil, RequestContext{PathStyle: true, Bucket: "bucket"})}
	for _, call := range []struct{ method, path string }{
		{"GET", "/bucket?list-type=2&prefix="}, {"PUT", "/bucket/object"}, {"HEAD", "/bucket/object"},
		{"POST", "/bucket/object?uploadId=upload"}, {"GET", "/bucket/object"},
	} {
		request, _ := http.NewRequest(call.method, server.URL+call.path, nil)
		response, err := client.Do(request)
		must(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(t, err)
		if call.method == "PUT" && string(body) != limited {
			t.Fatalf("reading the error code consumed the body: %q", body)
		}
	}
	history, err := f.controller.History(10)
	must(t, err)
	triggers := map[string]*RequestContext{}
	for _, event := range history {
		triggers[event.Type] = event.TriggerRequest
	}
	if len(history) != 3 || triggers["list"].StatusCode != 429 || triggers["list"].ErrorCode != "TooManyRequests" || triggers["head"].StatusCode != 429 ||
		triggers["put"].StatusCode != 400 || triggers["put"].ErrorCode != "RequestLimitExceeded" || triggers["put"].Message != "Storage service returned HTTP 400" || triggers["put"].RequestID != "trace-PUT" {
		t.Fatalf("429, a throttling code with 400 and HEAD 429 must each lower their own limit; 500 and object GETs nothing: %+v", history)
	}
	// list, put, head and the object GET; the 500 is not throttling.
	if got := autoAdjust(t, f.controller)["throttleCount"]; got != int64(4) {
		t.Fatalf("throttling responses counted %v, want 4", got)
	}
}

// The production incident: concurrent small PUTs, some throttled more often
// than the former three attempts allowed. Every object must still be written,
// and the node's PUT limit must come down.
func TestThrottledPutsAtHighConcurrencyFinishWithoutFailuresAndLowerTheNodeLimit(t *testing.T) {
	f := newFixture(t, Options{PutQPS: 100_000, HeadQPS: 100_000, MinQPS: 10, RecoveryInterval: time.Hour})
	// A frozen clock keeps the run within one window however slow the host.
	useClock(f.controller, time.Now())
	const objects, throttledAttempts = 400, 4
	var mu sync.Mutex
	attempts := map[string]int{}
	stored := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		_, exists := stored[key]
		switch r.Method {
		case "HEAD":
			if !exists {
				w.WriteHeader(404)
			}
		case "PUT":
			attempts[key]++
			index, _ := strconv.Atoi(strings.TrimPrefix(key, "object-"))
			if index%5 == 0 && attempts[key] <= throttledAttempts {
				w.WriteHeader(503)
				io.WriteString(w, `<Error><Code>SlowDown</Code><Message>Reduce your request rate.</Message></Error>`)
				return
			}
			if exists && r.Header.Get("If-None-Match") == "*" {
				w.WriteHeader(412)
				io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			stored[key] = body
			w.Header().Set("ETag", `"etag"`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 64
	defer base.CloseIdleConnections()
	store, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "fixture-ak", SecretKey: "fixture-sk", PathStyle: true, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: 2 * time.Millisecond, HTTPClient: &http.Client{Transport: f.controller.Transport(base, RequestContext{Bucket: "bucket", PathStyle: true, TaskID: 7})}})
	must(t, err)
	defer store.Close()
	work := make(chan int)
	var group sync.WaitGroup
	for range 64 {
		group.Go(func() {
			for index := range work {
				data := bytes.Repeat([]byte{byte(index)}, 16<<10)
				// Even objects take the conditional path: HEAD, then If-None-Match.
				result, err := store.Write(context.Background(), "object-"+strconv.Itoa(index), bytes.NewReader(data), transferstore.WriteOptions{Size: int64(len(data)), Overwrite: index%2 == 1})
				if err != nil || !result.Committed {
					t.Errorf("object %d failed: committed=%v %v", index, result.Committed, err)
				}
			}
		})
	}
	for index := range objects {
		work <- index
	}
	close(work)
	group.Wait()
	mu.Lock()
	for index := range objects {
		key := "object-" + strconv.Itoa(index)
		want := 1
		if index%5 == 0 {
			want = throttledAttempts + 1
		}
		if attempts[key] != want || !bytes.Equal(stored[key], bytes.Repeat([]byte{byte(index)}, 16<<10)) {
			t.Errorf("%s: %d PUT attempts, want %d, stored %d bytes", key, attempts[key], want, len(stored[key]))
		}
	}
	mu.Unlock()
	history, err := f.controller.History(10)
	must(t, err)
	if len(history) != 1 || history[0].Type != "put" || history[0].Value != 90_000 || rate(t, f.controller, "put")["state"] != "REDUCED" || rate(t, f.controller, "head")["limitQps"] != float64(100_000) {
		t.Fatalf("throttled PUTs must lower the node PUT limit once: %+v", history)
	}
}
