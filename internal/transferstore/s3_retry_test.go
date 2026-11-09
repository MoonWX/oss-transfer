package transferstore

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func responseError(status int, code string) error {
	return &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: &smithy.GenericAPIError{Code: code, Message: "test"}}
}

// ordinaryRetryable is an error the SDK retries that is neither throttling nor
// a transient failure, such as a probable clock skew.
type ordinaryRetryable struct{}

func (ordinaryRetryable) Error() string        { return "probable clock skew" }
func (ordinaryRetryable) RetryableError() bool { return true }

func TestS3RetryerGivesThrottlingItsOwnBudgetBackoffAndNoQuota(t *testing.T) {
	r := newS3Retryer(3, 10, time.Second, 20*time.Second)
	r.random = func() float64 { return 0.5 }
	slowDown, limited, tooMany := responseError(503, "SlowDown"), responseError(400, "RequestLimitExceeded"), responseError(429, "")
	var internal error = ordinaryRetryable{}
	denied := responseError(403, "AccessDenied")
	if r.MaxAttempts() != 10 || !r.IsErrorRetryable(slowDown) || !r.IsErrorRetryable(limited) || !r.IsErrorRetryable(tooMany) || !r.IsErrorRetryable(internal) || r.IsErrorRetryable(denied) {
		t.Fatal("wrong retry classification")
	}
	// Full jitter below a ceiling that doubles from 1 s and stops at 20 s.
	for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second, 6: 20 * time.Second, 9: 20 * time.Second} {
		if delay, err := r.RetryDelay(attempt, slowDown); err != nil || delay != ceiling/2 {
			t.Fatalf("attempt %d: delay %v, %v; want half of %v", attempt, delay, err, ceiling)
		}
	}
	r.random = func() float64 { return 0 }
	if delay, err := r.RetryDelay(4, tooMany); err != nil || delay != 0 {
		t.Fatalf("full jitter must reach zero: %v %v", delay, err)
	}
	var exhausted *retry.MaxAttemptsError
	if _, err := r.RetryDelay(10, limited); !errors.As(err, &exhausted) || exhausted.Attempt != 10 || !errors.Is(err, limited) {
		t.Fatalf("the tenth throttled attempt must be the last: %v", err)
	}
	// Ordinary errors keep three attempts.
	for attempt := 1; attempt < 3; attempt++ {
		if _, err := r.RetryDelay(attempt, internal); err != nil {
			t.Fatalf("ordinary attempt %d not retried: %v", attempt, err)
		}
	}
	if _, err := r.RetryDelay(3, internal); !errors.As(err, &exhausted) || exhausted.Attempt != 3 || !errors.Is(err, internal) {
		t.Fatalf("ordinary errors must stop after three attempts: %v", err)
	}
	// Throttled retries never exhaust the client's retry quota; others still do.
	for i := range 1000 {
		if _, err := r.GetRetryToken(context.Background(), slowDown); err != nil {
			t.Fatalf("throttled retry %d refused by the quota: %v", i, err)
		}
	}
	granted := 0
	for ; granted < 1000; granted++ {
		if _, err := r.GetRetryToken(context.Background(), internal); err != nil {
			break
		}
	}
	if granted == 1000 {
		t.Fatal("ordinary retries no longer use the retry quota")
	}
}

// scriptedS3 answers each key's attempts from a script: a status and error
// code per attempt, or success once the script runs out.
type scriptedS3 struct {
	mu       sync.Mutex
	scripts  map[string][]int
	attempts map[string]int
	server   *httptest.Server
}

var scriptedCodes = map[int]string{400: "RequestLimitExceeded", 429: "TooManyRequests", 500: "InternalError", 501: "NotImplemented", 502: "BadGateway", 503: "SlowDown"}

func newScriptedS3(t *testing.T, scripts map[string][]int) *scriptedS3 {
	t.Helper()
	s := &scriptedS3{scripts: scripts, attempts: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		s.mu.Lock()
		s.attempts[key]++
		attempt := s.attempts[key]
		script := s.scripts[key]
		s.mu.Unlock()
		if len(script) > 0 && (attempt <= len(script) || script[len(script)-1] < 0) {
			status := script[min(attempt, len(script))-1]
			if status < 0 {
				status = -status
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			io.WriteString(w, "<Error><Code>"+scriptedCodes[status]+"</Code><Message>scripted</Message></Error>")
			return
		}
		w.Header().Set("ETag", `"etag"`)
		if r.Method == "GET" {
			w.Header().Set("Content-Length", "4")
			io.WriteString(w, "data")
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *scriptedS3) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[key]
}

func (s *scriptedS3) store(t *testing.T) *S3 {
	t.Helper()
	store, err := NewS3(S3Config{Endpoint: s.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: time.Millisecond, HTTPClient: s.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the SDK's own backoff for ordinary errors out of the test's time.
	store.retryer.standard = retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = 3
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return time.Millisecond, nil })
		o.RateLimiter = store.retryer.quota
	})
	return store
}

func TestS3ThrottledRequestsOutlastTheOrdinaryAttemptLimit(t *testing.T) {
	// A negative last status repeats for every further attempt.
	server := newScriptedS3(t, map[string][]int{
		"throttled": {503, 503, 503, 503, 503, 503},
		"limited":   {400, 429, 400},
		"read":      {429, 503, 429},
		"broken":    {-501},
		"stuck":     {-503},
	})
	store := server.store(t)
	write := func(key string) error {
		_, err := store.Write(context.Background(), key, strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
		return err
	}
	for key, attempts := range map[string]int{"throttled": 7, "limited": 4} {
		if err := write(key); err != nil || server.count(key) != attempts {
			t.Fatalf("%s: %d attempts, %v; want success after %d", key, server.count(key), err, attempts)
		}
	}
	reader, _, err := store.Open(context.Background(), "read", OpenOptions{})
	if err != nil || server.count("read") != 4 {
		t.Fatalf("throttled GET: %d attempts, %v", server.count("read"), err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "data" {
		t.Fatalf("throttled GET returned %q, %v", data, err)
	}
	if err := write("broken"); err == nil || server.count("broken") != 1 {
		t.Fatalf("a request the server does not implement must not be retried: %d, %v", server.count("broken"), err)
	}
	if err := write("stuck"); err == nil || server.count("stuck") != 10 || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 10") || !strings.Contains(err.Error(), "SlowDown") {
		t.Fatalf("persistent throttling must end after ten attempts: %d, %v", server.count("stuck"), err)
	}
}

func TestS3ThrottleBackoffEndsWhenTheTransferStops(t *testing.T) {
	server := newScriptedS3(t, map[string][]int{"object": {-503}})
	store := server.store(t)
	// Every backoff would last about an hour.
	store.retryer.base, store.retryer.maxDelay = time.Hour, time.Hour
	backoff := make(chan struct{}, 1)
	store.retryer.random = func() float64 {
		select {
		case backoff <- struct{}{}:
		default:
		}
		return 0.999
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.Write(ctx, "object", strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
		done <- err
	}()
	<-backoff
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || server.count("object") != 1 {
			t.Fatalf("stopped transfer: %d attempts, %v", server.count("object"), err)
		}
	case <-time.After(time.Minute):
		t.Fatal("cancelling did not end the throttling backoff")
	}
}

func TestS3RetryerGivesTransientFailuresTheThrottlingBudget(t *testing.T) {
	r := newS3Retryer(3, 10, time.Second, 20*time.Second)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	reset := &url.Error{Op: "Put", URL: "http://storage/bucket/key", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	timeout := &url.Error{Op: "Get", URL: "http://storage/bucket/key", Err: &net.DNSError{Err: "i/o timeout", Name: "storage", IsTimeout: true}}
	temporaryDNS := &net.DNSError{Err: "server misbehaving", Name: "storage", IsTemporary: true}
	transient := map[string]error{"refused": refused, "reset": reset, "timeout": timeout, "temporary DNS": temporaryDNS,
		"500": responseError(500, "InternalError"), "502": responseError(502, ""), "504": responseError(504, "GatewayTimeout"), "request timeout": responseError(400, "RequestTimeout")}
	for name, err := range transient {
		if !isTransientError(err) || !r.IsErrorRetryable(err) {
			t.Fatalf("%s is not a transient failure", name)
		}
		// The quota never ends a transient retry.
		for i := range 1000 {
			if _, tokenErr := r.GetRetryToken(context.Background(), err); tokenErr != nil {
				t.Fatalf("%s retry %d refused by the quota: %v", name, i, tokenErr)
			}
		}
		// Waits stay between half the ceiling and the ceiling, which doubles
		// from 1 s and stops at 20 s.
		for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 20 * time.Second, 9: 20 * time.Second} {
			for _, draw := range []float64{0, 0.999} {
				r.random = func() float64 { return draw }
				delay, delayErr := r.RetryDelay(attempt, err)
				if delayErr != nil || delay < ceiling/2 || delay >= ceiling {
					t.Fatalf("%s attempt %d: delay %v, %v; want within [%v, %v)", name, attempt, delay, delayErr, ceiling/2, ceiling)
				}
			}
		}
		var exhausted *retry.MaxAttemptsError
		if _, delayErr := r.RetryDelay(10, err); !errors.As(delayErr, &exhausted) || exhausted.Attempt != 10 {
			t.Fatalf("%s: the tenth attempt must be the last: %v", name, delayErr)
		}
	}
	for name, err := range map[string]error{"no such host": &net.DNSError{Err: "no such host", Name: "bucket.storage", IsNotFound: true},
		"cancelled": &url.Error{Op: "Get", URL: "http://storage", Err: context.Canceled}, "501": responseError(501, "NotImplemented"),
		"denied": responseError(403, "AccessDenied"), "missing": responseError(404, "NoSuchKey")} {
		if isTransientError(err) || isPatientError(err) {
			t.Fatalf("%s must not get the long retry budget", name)
		}
	}
}

// refusingTransport refuses the first connections like a stopped storage
// service, then forwards requests to the server that has come back.
type refusingTransport struct {
	next    http.RoundTripper
	refuse  atomic.Int32
	dialled atomic.Int32
}

func (t *refusingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.dialled.Add(1)
	if t.refuse.Add(-1) >= 0 {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return t.next.RoundTrip(request)
}

func TestS3RidesOutAStorageOutageAndServerErrors(t *testing.T) {
	server := newScriptedS3(t, map[string][]int{"gateway": {502, 502, 502, 502, 502, 502}, "down": {-502}})
	transport := &refusingTransport{next: server.server.Client().Transport}
	store, err := NewS3(S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: time.Millisecond, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	write := func(key string) error {
		_, err := store.Write(context.Background(), key, strings.NewReader("payload"), WriteOptions{Size: 7, Overwrite: true})
		return err
	}
	// Seven refused connections: more than the three ordinary attempts.
	transport.refuse.Store(7)
	if err := write("restarted"); err != nil || transport.dialled.Load() != 8 || server.count("restarted") != 1 {
		t.Fatalf("outage: %d requests, %d reached the server, %v", transport.dialled.Load(), server.count("restarted"), err)
	}
	if err := write("gateway"); err != nil || server.count("gateway") != 7 {
		t.Fatalf("server errors: %d attempts, %v", server.count("gateway"), err)
	}
	if err := write("down"); err == nil || server.count("down") != 10 || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 10") {
		t.Fatalf("a persistent server error must end after ten attempts: %d, %v", server.count("down"), err)
	}
}

func TestConnectionChecksFailFastOnAnUnreachableStorage(t *testing.T) {
	server := newScriptedS3(t, nil)
	transport := &refusingTransport{next: server.server.Client().Transport}
	transport.refuse.Store(100)
	_, err := ListBuckets(context.Background(), S3Config{Endpoint: server.server.URL, AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, ThrottleRetryBaseDelay: time.Millisecond, ThrottleRetryMaxDelay: time.Millisecond, HTTPClient: &http.Client{Transport: transport}})
	if err == nil || !strings.Contains(err.Error(), "connection refused") || transport.dialled.Load() != 3 {
		t.Fatalf("connection check: %d attempts, %v", transport.dialled.Load(), err)
	}
}
