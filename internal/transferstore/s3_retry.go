package transferstore

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// Defaults for requests the storage service throttles. Ten attempts wait about
// a minute in total on average and at most 111 s (1+2+4+8+16+20+20+20+20 s),
// long enough for the node's automatic limit reduction to take effect.
// Transient network failures and server errors get the same ten attempts and
// ceilings; see s3Retryer.RetryDelay for their waits.
const (
	defaultThrottleRetryAttempts = 10
	defaultThrottleRetryBase     = time.Second
	defaultThrottleRetryMax      = 20 * time.Second
)

// throttleErrorCodes are the S3 and S3-compatible codes that ask a client to
// lower its request rate, whatever the HTTP status they arrive with.
var throttleErrorCodes = map[string]bool{
	"SlowDown":                  true,
	"Throttling":                true,
	"ThrottlingException":       true,
	"ThrottledException":        true,
	"RequestThrottled":          true,
	"RequestThrottledException": true,
	"RequestLimitExceeded":      true,
	"TooManyRequests":           true,
	"TooManyRequestsException":  true,
}

// IsThrottleResponse reports whether a storage response asks the client to slow
// down: HTTP 429 or 503, or a throttling error code with any status. The S3
// retryer and the node's request limits share this classification.
func IsThrottleResponse(status int, code string) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || throttleErrorCodes[code]
}

func isThrottleError(err error) bool {
	if err == nil {
		return false
	}
	code := ""
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		code = apiError.ErrorCode()
	}
	status := 0
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		status = response.HTTPStatusCode()
	}
	return IsThrottleResponse(status, code)
}

// transientErrorCodes are S3 codes for a request the service gave up waiting
// for, which a later attempt can complete.
var transientErrorCodes = map[string]bool{
	"RequestTimeout":          true,
	"RequestTimeoutException": true,
	"InternalError":           true,
	"ServiceUnavailable":      true,
}

// isTransientError reports a failure that a storage restart, failover or
// network interruption causes and that ends by itself: a refused, reset or
// aborted connection, a timeout, a temporary DNS failure, or an HTTP 5xx
// response other than 501 and 505, which never change on retry. A host name
// that does not exist is not transient, and neither is a cancelled request.
func isTransientError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		if status := response.HTTPStatusCode(); status >= 500 && status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported {
			return true
		}
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && transientErrorCodes[apiError.ErrorCode()] {
		return true
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return !dnsError.IsNotFound && (dnsError.IsTemporary || dnsError.IsTimeout)
	}
	for _, errno := range transientErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	var operation *net.OpError
	if errors.As(err, &operation) && strings.EqualFold(operation.Op, "dial") {
		return true
	}
	message := err.Error()
	for _, text := range []string{"connection refused", "connection reset", "broken pipe", "use of closed network connection"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// transientErrnos are the connection failures a restarting server causes.
// Windows reports reset and aborted connections with its own codes; a refused
// connection is a failed dial on every system.
var transientErrnos = append([]error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE}, platformTransientErrnos...)

// isPatientError reports the errors that get the long retry budget.
func isPatientError(err error) bool { return isThrottleError(err) || isTransientError(err) }

// s3Retryer keeps the SDK's standard retry policy, attempt limit and retry
// quota for ordinary errors, and gives throttled requests their own larger
// attempt limit with capped exponential backoff and full jitter. Transient
// network failures and server errors (see isTransientError) share that limit
// and the doubling ceilings, so a storage restart or failover of a minute or
// two is ridden out instead of failing every object in flight.
//
// Throttled retries never draw from the SDK's client-side retry quota. That
// token bucket belongs to one S3 client, which here is one child task, so under
// a throttling storm it would end retries after a fixed count unrelated to the
// service and turn every later throttled request into a failed object. Retry
// load stays bounded without it: every attempt is admitted by the node and task
// request limits, the backoff spreads retries over up to two minutes, and the
// node lowers its own limit while throttling lasts.
type s3Retryer struct {
	standard         *retry.Standard
	quota            *ratelimit.TokenRateLimit // the standard retryer's
	attempts         int                       // ordinary errors, including the first attempt
	throttleAttempts int
	base, maxDelay   time.Duration
	random           func() float64 // uniform in [0, 1)
}

func newS3Retryer(attempts, throttleAttempts int, base, maxDelay time.Duration) *s3Retryer {
	quota := ratelimit.NewTokenRateLimit(retry.DefaultRetryRateTokens)
	return &s3Retryer{
		standard: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = attempts
			o.RateLimiter = quota
		}),
		quota:            quota,
		attempts:         attempts,
		throttleAttempts: max(attempts, throttleAttempts),
		base:             base,
		maxDelay:         maxDelay,
		random:           rand.Float64,
	}
}

// retryCountKey holds one operation's count of retry decisions, which is the
// number of the attempt that has just failed.
type retryCountKey struct{}

func addS3RetryCount(stack *middleware.Stack) error {
	return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("S3RetryCount", func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
		return next.HandleInitialize(context.WithValue(ctx, retryCountKey{}, new(int)), in)
	}), middleware.Before)
}

func (r *s3Retryer) IsErrorRetryable(err error) bool {
	return isPatientError(err) || r.standard.IsErrorRetryable(err)
}

// MaxAttempts is the larger, throttling limit; RetryDelay ends ordinary errors
// at their own limit.
func (r *s3Retryer) MaxAttempts() int { return r.throttleAttempts }

// RetryDelay receives the number of attempts made so far. Stopping here returns
// the same error the SDK reports when it reaches MaxAttempts.
//
// A throttled retry waits a random time below the ceiling (full jitter), which
// spreads a node's retries while the service sheds load. A transient failure
// waits at least half the ceiling, so ten attempts span about one to two
// minutes whatever the random draws: long enough for a storage restart, where
// full jitter could end all attempts within seconds.
func (r *s3Retryer) RetryDelay(attempt int, err error) (time.Duration, error) {
	throttled := isThrottleError(err)
	if !throttled && !isTransientError(err) {
		if attempt >= r.attempts {
			return 0, &retry.MaxAttemptsError{Attempt: attempt, Err: err}
		}
		return r.standard.RetryDelay(attempt, err)
	}
	if attempt >= r.throttleAttempts {
		return 0, &retry.MaxAttemptsError{Attempt: attempt, Err: err}
	}
	ceiling := r.maxDelay
	if shift := attempt - 1; shift >= 0 && shift < 32 {
		if delay := r.base << shift; delay > 0 && delay < ceiling {
			ceiling = delay
		}
	}
	if throttled {
		return time.Duration(r.random() * float64(ceiling)), nil
	}
	return ceiling/2 + time.Duration(r.random()*float64(ceiling-ceiling/2)), nil
}

// GetRetryToken precedes every RetryDelay of an operation. Throttled and
// transient retries never take from the quota, and neither does an ordinary error's last attempt,
// which RetryDelay then ends: the SDK did not ask the quota for it either when
// MaxAttempts was the ordinary limit.
func (r *s3Retryer) GetRetryToken(ctx context.Context, err error) (func(error) error, error) {
	attempt := 0
	if count, ok := ctx.Value(retryCountKey{}).(*int); ok {
		*count++
		attempt = *count
	}
	if isPatientError(err) || attempt >= r.attempts {
		return func(error) error { return nil }, nil
	}
	return r.standard.GetRetryToken(ctx, err)
}

func (r *s3Retryer) GetInitialToken() func(error) error { return r.standard.GetInitialToken() }

func (r *s3Retryer) GetAttemptToken(ctx context.Context) (func(error) error, error) {
	return r.standard.GetAttemptToken(ctx)
}
