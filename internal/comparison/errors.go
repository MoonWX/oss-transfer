package comparison

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// maxErrorDetailBytes bounds Row.ErrorDetail and Progress.ErrorDetail.
const maxErrorDetailBytes = 512

// maxDetailKeyBytes bounds an object key quoted in a listing error.
const maxDetailKeyBytes = 256

// comparisonSideError names the side ("source" or "target") and the storage
// operation ("LIST", "HEAD" or "GET") that failed.
type comparisonSideError struct {
	side, operation string
	err             error
}

func (e *comparisonSideError) Error() string {
	return e.side + " " + e.operation + ": " + e.err.Error()
}

func (e *comparisonSideError) Unwrap() error { return e.err }

// comparisonListingError is a listing that a merge cannot use: unordered, out
// of the prefix or not advancing. Its message is built here, names the side and
// holds no storage response text, so ErrorDetail shows it as is.
type comparisonListingError struct{ message string }

func (e *comparisonListingError) Error() string { return e.message }

// comparisonTimeoutError is a storage call that exceeded one of the engine's
// own time limits while the comparison itself was still running. It does not
// unwrap to a context error, so callers never mistake it for cancellation.
type comparisonTimeoutError struct {
	operation string
	limit     time.Duration
	idle      bool
}

func (e *comparisonTimeoutError) Error() string {
	if e.idle {
		return e.operation + " received no data for " + e.limit.String()
	}
	return e.operation + " timed out after " + e.limit.String()
}

// errComparisonReadIdle is the cancellation cause of a read attempt that made
// no progress within readIdleTimeout.
var errComparisonReadIdle = errors.New("comparison read made no progress")

// ErrorDetail describes err for Row.ErrorDetail and Progress.ErrorDetail: the
// operation, side, HTTP status, S3 error code and request ID when known. It
// never includes credentials, signatures or request URLs.
//
// Run's own wrapping is kept as a prefix ("source LIST: ..."). Storage errors
// are reduced to their HTTP status, S3 error code and request ID, "timeout",
// "network error" or a transferstore sentinel name; transport and SDK messages,
// which can contain endpoints and object URLs, are never copied. Any other
// message is copied only after URLs and network addresses are removed. The
// result is at most 512 bytes of valid UTF-8 without control characters.
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	return clipDetail(cleanDetail(describeComparisonError(err)), maxErrorDetailBytes)
}

func describeComparisonError(err error) string {
	var side *comparisonSideError
	if errors.As(err, &side) {
		return side.side + " " + side.operation + ": " + describeComparisonError(side.err)
	}
	var listing *comparisonListingError
	if errors.As(err, &listing) {
		return listing.message
	}
	return describeStorageError(err)
}

func describeStorageError(err error) string {
	var timeout *comparisonTimeoutError
	if errors.As(err, &timeout) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	status, code, requestID := responseStatus(err), apiErrorCode(err), responseRequestID(err)
	var text string
	switch {
	case status >= 300 && code != "":
		text = "HTTP " + strconv.Itoa(status) + " " + code
	case status >= 300:
		text = "HTTP " + strconv.Itoa(status)
	case code != "":
		text = code
	default:
		text = describeLocalError(err)
	}
	if requestID != "" {
		text += " (request ID " + requestID + ")"
	}
	return text
}

// describeLocalError names an error that carries no storage error response.
func describeLocalError(err error) string {
	for _, sentinel := range []error{transferstore.ErrNotFound, transferstore.ErrSourceChanged, transferstore.ErrSizeMismatch, transferstore.ErrInvalidKey, transferstore.ErrInvalidCursor, transferstore.ErrUnsupported} {
		if errors.Is(err, sentinel) {
			return sentinel.Error()
		}
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "TLS certificate error"
	}
	if isNetworkError(err) {
		return "network error"
	}
	var decode *smithy.DeserializationError
	if errors.As(err, &decode) {
		return "invalid storage response"
	}
	var operation *smithy.OperationError
	if errors.As(err, &operation) {
		// SDK messages can embed the endpoint; keep only the operation name.
		if name := detailToken(operation.Operation(), 64, false); name != "" {
			return name + " failed"
		}
		return "storage request failed"
	}
	return redactDetail(err.Error())
}

// responseStatus is the HTTP status of a storage error response, or 0.
func responseStatus(err error) int {
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		// The SDK's HTTPStatusCode does not check for a missing response.
		if response.Response == nil || response.Response.Response == nil {
			return 0
		}
		return response.Response.StatusCode
	}
	var other interface{ HTTPStatusCode() int }
	if errors.As(err, &other) {
		return other.HTTPStatusCode()
	}
	return 0
}

// apiErrorCode is the S3 error code of a storage error response, or "".
func apiErrorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return detailToken(api.ErrorCode(), 64, false)
	}
	return ""
}

// responseRequestID is the request ID the storage service assigned to a failed
// request, from the SDK or from a provider's own request ID header.
func responseRequestID(err error) string {
	var aws *awshttp.ResponseError
	if errors.As(err, &aws) {
		if id := detailToken(aws.ServiceRequestID(), 128, true); id != "" {
			return id
		}
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.Response != nil && response.Response.Response != nil {
		for _, header := range []string{"X-Amz-Request-Id", "X-Oss-Request-Id", "X-Jdcloud-Request-Id", "X-Obs-Request-Id", "X-Cos-Request-Id"} {
			if id := detailToken(response.Response.Header.Get(header), 128, true); id != "" {
				return id
			}
		}
	}
	return ""
}

// detailToken keeps an identifier only when it is short and made of plain
// characters, so a hostile response cannot inject text into a detail.
func detailToken(value string, limit int, requestID bool) string {
	if value == "" || len(value) > limit {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' ||
			requestID && (r == '/' || r == '+' || r == '=')) {
			return ""
		}
	}
	return value
}

// isNetworkError reports a failure to send a request or to receive its whole
// response: connection errors, transport timeouts and truncated bodies.
func isNetworkError(err error) bool {
	var send *smithyhttp.RequestSendError
	var network net.Error
	return errors.As(err, &send) || errors.As(err, &network) || errors.Is(err, io.ErrUnexpectedEOF) ||
		retry.RetryableConnectionError{}.IsErrorRetryable(err) == aws.TrueTernary
}

// retryableStorageError reports a LIST failure worth another attempt: the
// engine's own attempt timeout, HTTP 5xx, 408 and 429, throttling and request
// timeout codes, network errors and a successful response whose body could not
// be read. Other HTTP errors, such as AccessDenied or NoSuchBucket, are final.
func retryableStorageError(err error) bool {
	var timeout *comparisonTimeoutError
	if errors.As(err, &timeout) {
		return true
	}
	status, code := responseStatus(err), apiErrorCode(err)
	if status >= 500 || status == http.StatusRequestTimeout || transferstore.IsThrottleResponse(status, code) {
		return true
	}
	if _, ok := retry.DefaultRetryableErrorCodes[code]; ok {
		return true
	}
	if _, ok := retry.DefaultThrottleErrorCodes[code]; ok {
		return true
	}
	if status >= 300 || code != "" {
		return false
	}
	var decode *smithy.DeserializationError
	return isNetworkError(err) || errors.As(err, &decode)
}

// finalReadError reports a content read failure that another attempt cannot
// fix: a changed, missing or differently sized object, an invalid key, and
// storage answers such as AccessDenied. Broken streams, timeouts, throttling
// and server errors are retried.
func finalReadError(err error) bool {
	for _, sentinel := range []error{transferstore.ErrSourceChanged, transferstore.ErrNotFound, transferstore.ErrSizeMismatch, transferstore.ErrInvalidKey, transferstore.ErrUnsupported} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	status := responseStatus(err)
	return status >= 300 && status < 500 && !retryableStorageError(err)
}

// objectChanged reports an object that changed or disappeared after it
// was listed or read.
func objectChanged(err error) bool {
	return errors.Is(err, transferstore.ErrSourceChanged) || errors.Is(err, transferstore.ErrNotFound)
}

func objectNotFound(err error) bool { return errors.Is(err, transferstore.ErrNotFound) }

var (
	detailURLPattern       = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]*`)
	detailIPv6Pattern      = regexp.MustCompile(`\[[0-9A-Fa-f:.%]*:[0-9A-Fa-f:.%]*\](?::\d+)?`)
	detailIPv4Pattern      = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	detailHostPortPattern  = regexp.MustCompile(`(?i)\b(?:(?:[a-z0-9-]+\.)+[a-z0-9-]+|localhost):\d{1,5}\b`)
	detailCredentialMarker = regexp.MustCompile(`(?i)\b(?:x-amz-[a-z-]+|signature|awsaccesskeyid|accesskey(?:id)?|access_key|secretkey|secret|token|password|credential)=[^\s&"']*`)
)

// redactDetail removes URLs, network addresses and credential parameters from
// a message that is not a known storage error.
func redactDetail(message string) string {
	message = detailURLPattern.ReplaceAllString(message, "<url>")
	message = detailCredentialMarker.ReplaceAllString(message, "<redacted>")
	message = detailIPv6Pattern.ReplaceAllString(message, "<address>")
	message = detailIPv4Pattern.ReplaceAllString(message, "<address>")
	return detailHostPortPattern.ReplaceAllString(message, "<address>")
}

// cleanDetail makes a detail valid UTF-8 on one line.
func cleanDetail(text string) string {
	text = strings.ToValidUTF8(text, "�")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}

// clipDetail shortens text to at most limit bytes on a UTF-8 boundary.
func clipDetail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := max(0, limit-3)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// comparisonSideDetail builds a row's ErrorDetail from the failing sides of one
// operation: "source HEAD: ...", "target HEAD: ..." or both.
func comparisonSideDetail(operation string, sourceErr, targetErr error) string {
	var parts []string
	if sourceErr != nil {
		parts = append(parts, ErrorDetail(&comparisonSideError{side: "source", operation: operation, err: sourceErr}))
	}
	if targetErr != nil {
		parts = append(parts, ErrorDetail(&comparisonSideError{side: "target", operation: operation, err: targetErr}))
	}
	if len(parts) == 2 {
		half := (maxErrorDetailBytes - 2) / 2
		return clipDetail(parts[0], half) + "; " + clipDetail(parts[1], half)
	}
	return strings.Join(parts, "; ")
}
