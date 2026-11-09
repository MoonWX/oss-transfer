package nodecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type fixture struct {
	path       string
	db         *state.Store
	limits     *ratelimit.Limiter
	controller *Controller
	options    Options
}

func newFixture(t *testing.T, options Options) *fixture {
	t.Helper()
	f := &fixture{path: filepath.Join(t.TempDir(), "agent.db"), options: options}
	f.open(t)
	t.Cleanup(f.close)
	return f
}
func (f *fixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.limits, err = ratelimit.New(f.db, ratelimit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.controller, err = New(f.db, f.limits, f.options)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) close() {
	if f.controller != nil {
		_ = f.controller.Close()
		f.controller = nil
	}
	if f.limits != nil {
		f.limits.Close()
		f.limits = nil
	}
	if f.db != nil {
		_ = f.db.Close()
		f.db = nil
	}
}

// restart reopens the Agent's database with other configured limits, as an
// edited service.json does.
func (f *fixture) restart(t *testing.T, change func(*Options)) {
	t.Helper()
	f.close()
	change(&f.options)
	f.open(t)
}
func rate(t *testing.T, c *Controller, kind string) map[string]any {
	t.Helper()
	result, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return result[kind].(map[string]any)
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersistentSettingsAutoIncidentRecoveryAndConfigurationSource(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 50, PutQPS: 100, HeadQPS: 200, Threshold: 2, ReduceFactor: .5, IncreaseFactor: 1.2, MinQPS: 10, RecoveryInterval: time.Hour})
	ctx := context.Background()
	must(t, f.controller.SetRoots(ctx, []string{filepath.Join(t.TempDir(), " root with spaces ")}))
	roots := f.controller.Config()
	must(t, f.controller.SetRate(ctx, "put", 70))
	if rate(t, f.controller, "put")["limitSource"] != "PERSISTED_MANUAL" {
		t.Fatal("manual override not identified")
	}
	must(t, f.controller.SetRate(ctx, "put", 100))
	if rate(t, f.controller, "put")["limitSource"] != "CONFIG" {
		t.Fatal("equal saved/configured value must show CONFIG")
	}
	must(t, f.controller.SetRate(ctx, "bandwidth", 8*32768))
	useClock(f.controller, time.Now())
	trigger := RequestContext{StatusCode: 503, Operation: "UploadPart", TaskID: 77, PartNumber: 2, RequestID: "request-123"}
	must(t, f.controller.ObserveResponse(trigger))
	if rate(t, f.controller, "put")["state"] != "NORMAL" {
		t.Fatal("warning appeared before reduction")
	}
	must(t, f.controller.ObserveResponse(RequestContext{StatusCode: 200, Operation: "UploadPart"}))
	must(t, f.controller.ObserveResponse(trigger))
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(50) || got["manualLimitQps"] != float64(100) || got["state"] != "REDUCED" {
		t.Fatalf("a success between throttling responses hid them, or wrong PUT reduction: %v", got)
	}
	must(t, f.controller.ObserveResponse(trigger))
	must(t, f.controller.ObserveResponse(trigger))
	if rate(t, f.controller, "put")["limitQps"] != float64(50) {
		t.Fatal("one burst lowered the limit twice within a window")
	}
	if rate(t, f.controller, "head")["limitQps"] != float64(200) || rate(t, f.controller, "list")["limitQps"] != float64(50) || f.limits.Node().BandwidthBytesPerSecond != 32768 {
		t.Fatal("throttled PUTs changed unrelated budgets")
	}
	history, err := f.controller.History(50)
	must(t, err)
	id := history[0].ID
	f.close()
	f.open(t)
	if !reflect.DeepEqual(roots, f.controller.Config()) || rate(t, f.controller, "put")["limitQps"] != float64(50) || f.limits.Node().BandwidthBytesPerSecond != 32768 {
		t.Fatal("restart lost settings or active reduction")
	}
	detail, err := f.controller.Detail(id, "")
	must(t, err)
	if !detail.ProcessRestarted || !detail.IncidentActive || detail.TriggerEvent == nil || detail.TriggerEvent.TriggerRequest.RequestID != "request-123" {
		t.Fatalf("trigger/history did not survive restart: %+v", detail)
	}
	now := time.Now().Add(2 * time.Hour)
	must(t, f.controller.Tick(now))
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(60) || got["state"] != "RECOVERING" {
		t.Fatalf("wrong recovery: %v", got)
	}
	for i := 0; i < 10; i++ {
		now = now.Add(2 * time.Hour)
		must(t, f.controller.Tick(now))
	}
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(100) || got["state"] != "RECOVERED" {
		t.Fatalf("did not restore exactly to target: %v", got)
	}
	detail, err = f.controller.Detail(id, "")
	must(t, err)
	// 50 → 60 → 72 → 86.4 → 100: the reduction and four recovery steps.
	if detail.IncidentActive || len(detail.Events) != 5 || !detail.Done || !detail.HistoryComplete {
		t.Fatalf("recovery timeline incomplete: %+v", detail)
	}
	if trigger := detail.TriggerEvent; trigger.ThrottleCount != 2 || trigger.ThrottleResponses != 3 || trigger.ThrottleThreshold != 2 || trigger.ThrottleRatio != 0.01 || trigger.ThrottleWindowMS != 5000 || trigger.Consecutive503Count != 0 {
		t.Fatalf("reduction did not record its window evidence: %+v", trigger)
	}
}

func TestActualHTTPAttemptsShareQPSAcrossClientsAndCancellationDoesNotReserve(t *testing.T) {
	f := newFixture(t, Options{PutQPS: 20})
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	clients := []*http.Client{{Transport: f.controller.Transport(nil, RequestContext{TaskID: 1})}, {Transport: f.controller.Transport(nil, RequestContext{TaskID: 2})}}
	start := time.Now()
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			request, _ := http.NewRequest("PUT", server.URL+"/object", nil)
			response, err := clients[i%2].Do(request)
			if err == nil {
				response.Body.Close()
			}
			errors <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		must(t, <-errors)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || count.Load() != 20 {
		t.Fatalf("separate clients bypassed shared QPS: elapsed=%s attempts=%d", elapsed, count.Load())
	}
	must(t, f.controller.SetRate(context.Background(), "put", .01))
	must(t, f.controller.Wait(context.Background(), "put"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.controller.Wait(ctx, "put"); err != context.DeadlineExceeded {
		t.Fatalf("wait not cancelled: %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- f.controller.Wait(context.Background(), "put") }()
	must(t, f.controller.SetRate(context.Background(), "put", 1000))
	select {
	case err := <-result:
		must(t, err)
	case <-time.After(time.Second):
		t.Fatal("policy change failed to wake waiter")
	}
	f.controller.mu.Lock()
	samples := f.controller.samples
	f.controller.mu.Unlock()
	var attempts int64
	for _, sample := range samples {
		attempts += sample.values[1]
	}
	if attempts != 22 {
		t.Fatalf("cancelled call charged an attempt: %d", attempts)
	}
}

func TestNodeBandwidthSharesAllParentsAndMeasuresSuccessfulReads(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	const size = 128 << 10
	must(t, f.controller.SetRate(ctx, "bandwidth", 8*256*1024))
	for id := int64(1); id <= 2; id++ {
		must(t, f.limits.ApplyParent(ctx, ratelimit.ParentLimit{ParentID: id, Generation: 1, Revision: 1, BandwidthBytesPerSecond: 16 << 20}))
	}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for id := int64(1); id <= 2; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte(id)}, size)
			reader, err := f.controller.Decorator(f.limits.Decorator)(ctx, execution.Job{ParentID: id, Generation: 1}, execution.Item{Source: transferstore.Object{Size: size}}, io.NopCloser(bytes.NewReader(data)))
			if err != nil {
				errs <- err
				return
			}
			output, err := io.ReadAll(reader)
			reader.Close()
			if err == nil && !bytes.Equal(output, data) {
				err = io.ErrUnexpectedEOF
			}
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("node cap was applied separately per parent: %s", elapsed)
	}
	f.controller.mu.Lock()
	var measured int64
	for _, s := range f.controller.samples {
		measured += s.values[3]
	}
	f.controller.mu.Unlock()
	if measured != 2*size {
		t.Fatalf("throughput counted reservations/EOF instead of successful bytes: %d", measured)
	}
	// Completed-bucket arithmetic uses actual read counters and repeated reads do
	// not consume/reset them. Moving timestamps avoids a five-second test delay.
	f.controller.mu.Lock()
	for i := range f.controller.samples {
		if f.controller.samples[i].values[3] > 0 {
			f.controller.samples[i].second = time.Now().Unix() - 1
		}
	}
	f.controller.mu.Unlock()
	first := rate(t, f.controller, "bandwidth")["currentBps"]
	second := rate(t, f.controller, "bandwidth")["currentBps"]
	if first != float64(2*size*8)/5 || first != second {
		t.Fatalf("measurements reset or use bytes instead of bits: %v %v", first, second)
	}
}

func TestInvalidOrFailedConfigurationNeverPublishes(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1), .001} {
		if err := f.controller.SetRate(ctx, "put", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid rate accepted: %v", err)
		}
	}
	if err := f.controller.SetRoots(ctx, []string{"relative"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("relative root accepted")
	}
	must(t, f.db.Close())
	if err := f.controller.SetRate(ctx, "put", 42); err == nil {
		t.Fatal("save to closed DB reported success")
	}
	f.controller.mu.Lock()
	active := f.controller.data.Effective["put"]
	f.controller.mu.Unlock()
	if active != 1400 {
		t.Fatal("failed write changed active rate")
	}
	f.db = nil
}

// A saved effective rate that is unusable or unlimited under a limit neither
// disables the limit nor stops the Agent: the limit starts at its target, and
// a reduction left at the target by an earlier version ends.
func TestStartupResetsUnusableEffectiveRateAndRejectsInvalidAutomaticPolicy(t *testing.T) {
	f := newFixture(t, Options{})
	must(t, f.controller.Close())
	f.controller = nil
	must(t, f.db.Update(func(tx *state.Tx) error {
		var saved persisted
		_, err := tx.Get("node-control", "settings", &saved)
		if err != nil {
			return err
		}
		// Below the 0.01/s minimum, unlimited, at the target.
		saved.Effective["list"], saved.Effective["put"], saved.Effective["head"], saved.Effective["bandwidth"] = 0.001, 0, 2800, 0
		saved.Incidents = map[string]string{"list": "old-1", "put": "old-2", "head": "old-3"}
		return tx.Put("node-control", "settings", saved)
	}))
	for _, options := range []Options{{ReduceFactor: math.NaN()}, {IncreaseFactor: math.Inf(1)}, {MinQPS: math.NaN()}} {
		if c, err := New(f.db, f.limits, options); err == nil {
			c.Close()
			t.Fatal("invalid automatic policy accepted")
		}
	}
	var err error
	f.controller, err = New(f.db, f.limits, Options{})
	must(t, err)
	for kind, want := range map[string]float64{"list": 100, "put": 1400, "head": 2800} {
		if got := rate(t, f.controller, kind); got["limitQps"] != want || got["state"] != "NORMAL" {
			t.Fatalf("%s did not start at its target over an unusable saved rate: %v", kind, got)
		}
	}
	if _, pending := autoAdjust(t, f.controller)["nextRecoveryAt"]; pending {
		t.Fatal("a reduction continued for a limit at its target")
	}
	if got := f.limits.Node().BandwidthBytesPerSecond; got != 250_000_000 {
		t.Fatalf("bandwidth did not start at its target over an unusable saved rate: %d bytes/s", got)
	}
}

func TestRestartAppliesConfiguredRequestLimits(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 0, PutQPS: 100, HeadQPS: 100, UseZeroDefaults: true, MinQPS: 10, RecoveryInterval: time.Hour})
	list := func(limit, target float64, state string) {
		t.Helper()
		got := rate(t, f.controller, "list")
		if got["limitQps"] != limit || got["manualLimitQps"] != target || got["configuredLimitQps"] != target || got["limitSource"] != "CONFIG" || got["state"] != state {
			t.Fatalf("list: want limit %v, configured target %v, %s; got %v", limit, target, state, got)
		}
	}
	// A limit that was off is turned on at once.
	f.restart(t, func(o *Options) { o.ListQPS = 50 })
	list(50, 50, "NORMAL")
	if got := f.controller.TaskRequests(1, 1).NodeLimits.List; got != 50 {
		t.Fatalf("LIST requests are not limited to the configured 50/s: %v", got)
	}
	// Lowered at once.
	f.restart(t, func(o *Options) { o.ListQPS = 20 })
	list(20, 20, "NORMAL")
	// Raised in recovery steps.
	f.restart(t, func(o *Options) { o.ListQPS = 40 })
	list(20, 40, "REDUCED")
	must(t, f.controller.Tick(time.Now().Add(2*time.Hour)))
	list(24, 40, "RECOVERING")
	// Turned off at once.
	f.restart(t, func(o *Options) { o.ListQPS = 0 })
	list(0, 0, "UNLIMITED")

	// Turning off a limit during an automatic reduction ends the reduction.
	useClock(f.controller, time.Now())
	for range 5 {
		must(t, f.controller.ObserveResponse(RequestContext{Operation: "PutObject", StatusCode: 503}))
	}
	history, err := f.controller.History(1)
	must(t, err)
	if len(history) != 1 || history[0].Type != "put" || history[0].IncidentID == "" || rate(t, f.controller, "put")["state"] != "REDUCED" {
		t.Fatalf("PUT was not reduced: %+v", history)
	}
	f.restart(t, func(o *Options) { o.PutQPS = 0 })
	detail, err := f.controller.Detail(history[0].ID, "")
	must(t, err)
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(0) || got["state"] != "UNLIMITED" || detail.IncidentActive {
		t.Fatalf("PUT turned off still reduced: %v, incident active %v", got, detail.IncidentActive)
	}
	if _, pending := autoAdjust(t, f.controller)["nextRecoveryAt"]; pending {
		t.Fatal("recovery still scheduled for a limit that is off")
	}
	f.restart(t, func(o *Options) { o.PutQPS = 100 })
	if got := rate(t, f.controller, "put"); got["limitQps"] != float64(100) || got["state"] != "NORMAL" {
		t.Fatalf("PUT turned on again: %v", got)
	}
}

// Bandwidth is never lowered automatically: a configured change applies on
// every start, not only the first one.
func TestRestartAppliesConfiguredBandwidth(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 100, PutQPS: 100, HeadQPS: 100, BandwidthBitsPerSecond: 0, UseZeroDefaults: true, RecoveryInterval: time.Hour})
	for _, step := range []struct{ configured, enforced float64 }{
		{0, 0}, {1e9, 1e9}, {4e8, 4e8}, {0, 0}, {2e9, 2e9},
		// The limiter enforces whole bytes per second.
		{500_000_004, 500_000_000},
	} {
		f.restart(t, func(o *Options) { o.BandwidthBitsPerSecond = step.configured })
		if got := f.limits.Node().BandwidthBytesPerSecond; got != int64(step.enforced/8) {
			t.Fatalf("configured %v bit/s: limiter enforces %d bytes/s", step.configured, got)
		}
		state := "NORMAL"
		if step.enforced == 0 {
			state = "UNLIMITED"
		}
		if got := rate(t, f.controller, "bandwidth"); got["limitBps"] != step.enforced || got["manualLimitBps"] != step.enforced || got["configuredLimitBps"] != step.enforced || got["limitSource"] != "CONFIG" || got["state"] != state {
			t.Fatalf("configured %v bit/s is not shown as the configured target: %v", step.configured, got)
		}
	}
}

func TestSavedLimitsKeepTheirValueWhenTheConfigurationChanges(t *testing.T) {
	f := newFixture(t, Options{ListQPS: 0, PutQPS: 100, HeadQPS: 100, BandwidthBitsPerSecond: 1e9, UseZeroDefaults: true, RecoveryInterval: time.Hour})
	ctx := context.Background()
	must(t, f.controller.SetRate(ctx, "list", 30))
	must(t, f.controller.SetRate(ctx, "put", 0))
	must(t, f.controller.SetRate(ctx, "bandwidth", 8e6))
	// Saving the configured value shows the configured target, but stays saved.
	must(t, f.controller.SetRate(ctx, "head", 50))
	must(t, f.controller.SetRate(ctx, "head", 100))
	if got := rate(t, f.controller, "head")["limitSource"]; got != "CONFIG" {
		t.Fatalf("saved value equal to the configured one: %v", got)
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.ListQPS, o.PutQPS, o.HeadQPS, o.BandwidthBitsPerSecond = 500, 200, 300, 2e9 },
		func(o *Options) { o.ListQPS, o.PutQPS, o.HeadQPS, o.BandwidthBitsPerSecond = 0, 50, 0, 0 },
	} {
		f.restart(t, change)
		for kind, want := range map[string]float64{"list": 30, "put": 0, "head": 100} {
			if got := rate(t, f.controller, kind); got["limitQps"] != want || got["manualLimitQps"] != want || got["limitSource"] != "PERSISTED_MANUAL" {
				t.Fatalf("saved %s limit changed with the configuration: %v", kind, got)
			}
		}
		if got := rate(t, f.controller, "bandwidth"); f.limits.Node().BandwidthBytesPerSecond != 1e6 || got["limitBps"] != 8e6 || got["manualLimitBps"] != 8e6 || got["limitSource"] != "PERSISTED_MANUAL" {
			t.Fatalf("saved bandwidth changed with the configuration: %v", got)
		}
	}
}

func TestHTTPFailureMetadataIsUsefulWithoutLeakingCredentialsAndBodyIsReplayed(t *testing.T) {
	f := newFixture(t, Options{PutQPS: 100, HeadQPS: 100, Threshold: 1, MinQPS: 10})
	body := `<Error><Code>SlowDown</Code><Message>secret-from-response</Message></Error>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Request-Id", "trace-42")
		w.WriteHeader(503)
		io.WriteString(w, body)
	}))
	defer server.Close()
	client := &http.Client{Transport: f.controller.Transport(nil, RequestContext{PathStyle: true, Bucket: "bucket", ConnectionAlias: "test", TaskID: 7, Direction: "LOCAL_TO_OSS"})}
	r, _ := http.NewRequest("PUT", server.URL+"/bucket/path/object?partNumber=3&uploadId=secret-upload-token&X-Amz-Signature=secret-query", nil)
	r.Header.Set("Authorization", "secret-authorization")
	response, err := client.Do(r)
	must(t, err)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if string(data) != body {
		t.Fatal("observing error consumed SDK response body")
	}
	history, err := f.controller.History(10)
	must(t, err)
	trigger := history[0].TriggerRequest
	if trigger == nil || trigger.Operation != "UploadPart" || trigger.PartNumber != 3 || trigger.Key != "path/object" || trigger.RequestID != "trace-42" || trigger.ErrorCode != "SlowDown" || trigger.TaskID != 7 {
		t.Fatalf("wrong trigger context: %+v", trigger)
	}
	encoded, err := json.Marshal(history)
	must(t, err)
	for _, secret := range []string{"secret-upload-token", "secret-query", "secret-authorization", "secret-from-response"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("secret copied into adjustment history")
		}
	}
}

// This uses the real S3 SDK against a deliberately faulting HTTP endpoint. It
// proves placement below SDK retries, not compatibility with a cloud provider.
func TestSDKMultipartRetrySpendsEveryActualAttempt(t *testing.T) {
	f := newFixture(t, Options{PutQPS: 1000, HeadQPS: 1000, Threshold: 1, MinQPS: 10})
	must(t, f.limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: 7, Generation: 1, Revision: 1, PutQPS: 10}))
	parent := &recordingParentLimiter{Limiter: f.limits, admitted: map[requestAdmission]int{}}
	f.controller.mu.Lock()
	f.controller.bytes = parent
	f.controller.mu.Unlock()
	var attempts atomic.Int64
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		io.Copy(io.Discard, r.Body)
		q := r.URL.Query()
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT" && q.Has("partNumber"):
			if !failed.Swap(true) {
				w.WriteHeader(503)
				io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
				return
			}
			w.Header().Set("ETag", `"part"`)
		case r.Method == "POST":
			io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"final"</ETag></CompleteMultipartUploadResult>`)
		default:
			t.Errorf("unexpected SDK request: %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	store, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "fixture-ak", SecretKey: "fixture-sk", PathStyle: true, PartSize: 5 << 20, RetryMaxAttempts: 2, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: time.Millisecond, HTTPClient: &http.Client{Transport: f.controller.Transport(nil, RequestContext{Bucket: "bucket", PathStyle: true, TaskID: 7, Generation: 1})}})
	must(t, err)
	defer store.Close()
	data := bytes.Repeat([]byte{7}, (5<<20)+1)
	result, err := store.Write(context.Background(), "object", bytes.NewReader(data), transferstore.WriteOptions{Size: int64(len(data)), Overwrite: true})
	must(t, err)
	if !result.Committed || attempts.Load() != 5 {
		t.Fatalf("SDK did not perform initiation, 2 parts, retry, completion: attempts=%d", attempts.Load())
	}
	f.controller.mu.Lock()
	var measured int64
	for _, sample := range f.controller.samples {
		measured += sample.values[1]
	}
	f.controller.mu.Unlock()
	// Observe successful admission by the real limiter. Server arrival times
	// can bunch after admission when a goroutine or network write is delayed.
	// The limiter's rate and joint-budget atomicity have separate tests.
	parent.mu.Lock()
	if len(parent.admitted) != 1 || parent.admitted[requestAdmission{7, 1, "put"}] != int(attempts.Load()) {
		t.Errorf("SDK multipart/retry bypassed parent admission: admitted=%v actual=%d", parent.admitted, attempts.Load())
	}
	parent.mu.Unlock()
	if measured != attempts.Load() {
		t.Fatalf("SDK retry bypassed node limiter: measured=%d actual=%d", measured, attempts.Load())
	}
	history, err := f.controller.History(10)
	must(t, err)
	if len(history) != 1 || history[0].Type != "put" || history[0].TriggerRequest.Operation != "UploadPart" {
		t.Fatalf("actual retry failure not recorded as a PUT reduction: %+v", history)
	}
}

type requestAdmission struct {
	parentID   int64
	generation uint64
	kind       string
}

type recordingParentLimiter struct {
	*ratelimit.Limiter
	mu       sync.Mutex
	admitted map[requestAdmission]int
}

func (l *recordingParentLimiter) TryRequest(parentID int64, generation uint64, kind string, notBefore time.Time) (time.Duration, <-chan struct{}, error) {
	delay, changed, err := l.Limiter.TryRequest(parentID, generation, kind, notBefore)
	if err == nil && delay <= 0 {
		l.mu.Lock()
		l.admitted[requestAdmission{parentID, generation, kind}]++
		l.mu.Unlock()
	}
	return delay, changed, err
}
