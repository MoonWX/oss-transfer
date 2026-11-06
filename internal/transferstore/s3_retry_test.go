package transferstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func responseError(status int, code string) error {
	return &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: &smithy.GenericAPIError{Code: code, Message: "test"}}
}

func TestS3RetryerGivesThrottlingItsOwnBudgetBackoffAndNoQuota(t *testing.T) {
	r := newS3Retryer(3, 10, time.Second, 20*time.Second)
	r.random = func() float64 { return 0.5 }
	slowDown, limited, tooMany := responseError(503, "SlowDown"), responseError(400, "RequestLimitExceeded"), responseError(429, "")
	internal, denied := responseError(500, "InternalError"), responseError(403, "AccessDenied")
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

var scriptedCodes = map[int]string{400: "RequestLimitExceeded", 429: "TooManyRequests", 500: "InternalError", 503: "SlowDown"}

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
		"broken":    {-500},
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
	if err := write("broken"); err == nil || server.count("broken") != 3 || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 3") {
		t.Fatalf("ordinary errors must keep three attempts: %d, %v", server.count("broken"), err)
	}
	// Two retries took from the quota, as before; the last attempt did not.
	if got := store.retryer.quota.Remaining(); got != retry.DefaultRetryRateTokens-2*retry.DefaultRetryCost {
		t.Fatalf("retry quota left %d after one failed operation", got)
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
