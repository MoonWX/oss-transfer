package comparison

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func engineNetworkError() error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: "GetObject", Err: &smithyhttp.RequestSendError{Err: &url.Error{
		Op:  "Get",
		URL: "https://secret-bucket.storage.example.com:9000/key?X-Amz-Credential=AKIDEXAMPLE&X-Amz-Signature=deadbeef",
		Err: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 9000}, Err: errors.New("connection refused")},
	}}}
}

func TestEngineErrorDetailKeepsStorageFieldsAndRedacts(t *testing.T) {
	headerOnly := &http.Response{StatusCode: 503, Header: http.Header{"X-Oss-Request-Id": {"OSS-REQ-1"}}}
	hostile := &http.Response{StatusCode: 403, Header: http.Header{"X-Amz-Request-Id": {"id with space https://evil.example.com"}}}
	certificate := &smithyhttp.RequestSendError{Err: &url.Error{Op: "Get", URL: "https://storage.example.com/b", Err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}}}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"api error", &comparisonSideError{side: "target", operation: "HEAD", err: engineAPIError(403, "AccessDenied", "1A2B")}, "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)"},
		{"wrapped run error", fmt.Errorf("comparison: %w", &comparisonSideError{side: "source", operation: "LIST", err: engineAPIError(404, "NoSuchBucket", "R-404")}), "source LIST: HTTP 404 NoSuchBucket (request ID R-404)"},
		{"joined with close error", errors.Join(&comparisonSideError{side: "source", operation: "LIST", err: engineAPIError(500, "InternalError", "R-500")}, errors.New("close failed")), "source LIST: HTTP 500 InternalError (request ID R-500)"},
		{"provider request ID header", &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: headerOnly}, Err: errors.New("service unavailable")}, "HTTP 503 (request ID OSS-REQ-1)"},
		{"hostile request ID and code", &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: hostile}, Err: &smithy.GenericAPIError{Code: "Bad Code <x>"}}, RequestID: "a b"}, "HTTP 403"},
		{"network", &comparisonSideError{side: "source", operation: "GET", err: engineNetworkError()}, "source GET: network error"},
		{"unexpected EOF", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), "network error"},
		{"certificate", certificate, "TLS certificate error"},
		{"engine timeout", &comparisonSideError{side: "target", operation: "HEAD", err: &comparisonTimeoutError{operation: "HEAD", limit: time.Minute}}, "target HEAD: timeout"},
		{"idle timeout", &comparisonTimeoutError{operation: "GET", limit: time.Minute, idle: true}, "timeout"},
		{"deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), "timeout"},
		{"not found", fmt.Errorf("%w: src/a", transferstore.ErrNotFound), "object not found"},
		{"changed", fmt.Errorf("%w: etag", transferstore.ErrSourceChanged), "source changed"},
		{"size", fmt.Errorf("%w: short", transferstore.ErrSizeMismatch), "object size mismatch"},
		{"listing", &comparisonListingError{message: `source listing is not in ascending byte order after key "a"`}, `source listing is not in ascending byte order after key "a"`},
		{"successful status with bad body", &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 200, Header: http.Header{}}}, Err: &smithy.DeserializationError{Err: errors.New("xml syntax error")}}, RequestID: "R-200"}, "invalid storage response (request ID R-200)"},
		{"sdk operation", &smithy.OperationError{ServiceID: "S3", OperationName: "HeadObject", Err: errors.New("endpoint https://minio.internal:9000 rejected")}, "HeadObject failed"},
		{"plain", errors.New("invalid comparison request"), "invalid comparison request"},
		{"response error without a response", &smithyhttp.ResponseError{Err: &smithy.GenericAPIError{Code: "SlowDown"}}, "SlowDown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorDetail(test.err); got != test.want {
				t.Fatalf("ErrorDetail = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEngineErrorDetailRedactsUnknownMessages(t *testing.T) {
	raw := `Get "https://AKIDEXAMPLE:topsecret@bucket.storage.example.com:9000/k?X-Amz-Credential=AKIDEXAMPLE%2F2026&X-Amz-Signature=deadbeef": dial tcp 10.1.2.3:9000 and [fd00::7]:443 via proxy.example.com:3128 token=abcd` + "\nnext line"
	got := ErrorDetail(&comparisonSideError{side: "source", operation: "GET", err: errors.New(raw)})
	for _, secret := range []string{"AKIDEXAMPLE", "topsecret", "storage.example.com", "deadbeef", "10.1.2.3", "fd00", "proxy.example.com", "abcd", "https", "\n"} {
		if strings.Contains(got, secret) {
			t.Fatalf("detail kept %q: %q", secret, got)
		}
	}
	if !strings.HasPrefix(got, "source GET: Get \"<url>\": dial tcp <address>") {
		t.Fatalf("redaction removed the useful text: %q", got)
	}
	long := ErrorDetail(&comparisonSideError{side: "target", operation: "LIST", err: errors.New(strings.Repeat("é", 2000) + "\x00\x7f")})
	if len(long) > maxErrorDetailBytes || !utf8.ValidString(long) || !strings.HasPrefix(long, "target LIST: é") || !strings.HasSuffix(long, "...") {
		t.Fatalf("detail not bounded to valid UTF-8: %d bytes %q", len(long), long[len(long)-10:])
	}
	if got := ErrorDetail(errors.New("bad\x00key\r\nline")); got != "bad key  line" {
		t.Fatalf("control characters kept: %q", got)
	}
	if got := ErrorDetail(errors.New(string([]byte{'a', 0xff, 'b'}))); got != "a�b" {
		t.Fatalf("invalid UTF-8 kept: %q", got)
	}
	both := comparisonSideDetail("HEAD", errors.New(strings.Repeat("s", 600)), errors.New(strings.Repeat("t", 600)))
	if len(both) > maxErrorDetailBytes || !strings.Contains(both, "; target HEAD: ttt") {
		t.Fatalf("two-sided detail lost the second side or exceeded the limit: %d %q", len(both), both)
	}
}

func TestEngineRetryClassification(t *testing.T) {
	send := &smithyhttp.RequestSendError{Err: errors.New("connection reset by peer")}
	badBody := &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 200, Header: http.Header{}}}, Err: &smithy.DeserializationError{Err: io.ErrUnexpectedEOF}}}
	for _, test := range []struct {
		name             string
		err              error
		retryList, final bool
	}{
		{"500", engineAPIError(500, "InternalError", ""), true, false},
		{"502 without code", &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 502, Header: http.Header{}}}, Err: errors.New("bad gateway")}, true, false},
		{"503 SlowDown", engineAPIError(503, "SlowDown", ""), true, false},
		{"429", engineAPIError(429, "TooManyRequests", ""), true, false},
		{"408", engineAPIError(408, "RequestTimeout", ""), true, false},
		{"400 RequestTimeout", engineAPIError(400, "RequestTimeout", ""), true, false},
		{"400 throttling code", engineAPIError(400, "Throttling", ""), true, false},
		{"403 AccessDenied", engineAPIError(403, "AccessDenied", ""), false, true},
		{"403 InvalidAccessKeyId", engineAPIError(403, "InvalidAccessKeyId", ""), false, true},
		{"403 SignatureDoesNotMatch", engineAPIError(403, "SignatureDoesNotMatch", ""), false, true},
		{"404 NoSuchBucket", engineAPIError(404, "NoSuchBucket", ""), false, true},
		{"400 InvalidArgument", engineAPIError(400, "InvalidArgument", ""), false, true},
		{"301 redirect", engineAPIError(301, "PermanentRedirect", ""), false, true},
		{"request send", send, true, false},
		{"network", engineNetworkError(), true, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, true, false},
		{"bad successful body", badBody, true, false},
		{"engine timeout", &comparisonTimeoutError{operation: "LIST", limit: time.Minute}, true, false},
		{"invalid cursor", fmt.Errorf("%w: rejected", transferstore.ErrInvalidCursor), false, false},
		{"source changed", transferstore.ErrSourceChanged, false, true},
		{"not found", fmt.Errorf("%w: k", transferstore.ErrNotFound), false, true},
		{"size mismatch", transferstore.ErrSizeMismatch, false, true},
		{"unsupported", transferstore.ErrUnsupported, false, true},
		{"unknown", errors.New("something odd"), false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableStorageError(test.err); got != test.retryList {
				t.Fatalf("retryableStorageError = %v", got)
			}
			if got := finalReadError(test.err); got != test.final {
				t.Fatalf("finalReadError = %v", got)
			}
		})
	}
}

func TestEngineSameVersionIgnoresSubsecondTimes(t *testing.T) {
	listed := time.Date(2026, 1, 1, 0, 0, 0, 123_000_000, time.UTC)
	head := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := func(size int64, etag, version string, modified time.Time) transferstore.Object {
		return transferstore.Object{Key: "k", Size: size, ETag: etag, VersionID: version, ModifiedAt: modified}
	}
	for _, test := range []struct {
		name string
		a, b transferstore.Object
		want bool
	}{
		{"ETags decide, not milliseconds", o(4, `"e1"`, "", listed), o(4, "e1", "", head), true},
		{"ETags decide, not whole seconds", o(4, `"e1"`, "", head), o(4, `"e1"`, "", head.Add(time.Hour)), true},
		{"different ETag", o(4, `"e1"`, "", head), o(4, `"e2"`, "", head), false},
		{"different size", o(4, `"e1"`, "", head), o(5, `"e1"`, "", head), false},
		{"different versions", o(4, `"e1"`, "v1", head), o(4, `"e1"`, "v2", head), false},
		{"one version missing", o(4, `"e1"`, "v1", head), o(4, `"e1"`, "", head), true},
		{"versions without ETags", o(4, "", "v1", listed), o(4, "", "v1", head.Add(time.Hour)), true},
		{"times truncated to seconds", o(4, "", "", listed), o(4, "", "", head), true},
		{"later second", o(4, "", "", listed), o(4, "", "", head.Add(time.Second)), false},
		{"one ETag only falls back to time", o(4, `"e1"`, "", listed), o(4, "", "", head), true},
		{"no reference time", o(4, "", "", time.Time{}), o(4, "", "", head), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameVersion(test.a, test.b); got != test.want {
				t.Fatalf("sameVersion = %v", got)
			}
		})
	}
}

func TestEngineMetadataReasonsAndETagRule(t *testing.T) {
	base := transferstore.Object{Size: 4, ContentType: "text/plain", ContentEncoding: "gzip", ContentDisposition: "inline", CacheControl: "no-cache", Metadata: map[string]string{"a": "1"}}
	other := transferstore.Object{Size: 5, ContentType: "text/html", ContentEncoding: "br", ContentDisposition: "attachment", CacheControl: "max-age=1", Metadata: map[string]string{"a": "2"}}
	want := []string{ReasonSize, ReasonContentType, ReasonContentEncoding, ReasonContentDisposition, ReasonCacheControl, ReasonMetadata}
	if got := metadataReasons(base, other); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reasons = %v", got)
	}
	if got := metadataReasons(transferstore.Object{Size: 1}, transferstore.Object{Size: 1, Metadata: map[string]string{}}); len(got) != 0 {
		t.Fatalf("empty and missing metadata differ: %v", got)
	}
	const md5a, md5b = "0cc175b9c0f1b6a831c399e269772661", "92eb5ffee6ae2fec3ad71c777531578f"
	for _, test := range []struct {
		a, b string
		want bool
	}{
		{`"` + md5a + `"`, md5b, true},
		{md5a, strings.ToUpper(md5a), false},
		{`"` + md5a + `"`, `"` + md5a + `"`, false},
		{md5a + "-2", md5b + "-2", false},
		{md5a, md5b + "-1", false},
		{"opaque", md5b, false},
		{"", md5b, false},
		{md5a[:31] + "g", md5b, false},
	} {
		if got := md5ETagsDiffer(test.a, test.b); got != test.want {
			t.Fatalf("md5ETagsDiffer(%q, %q) = %v", test.a, test.b, got)
		}
	}
}

func TestEngineClipDetailKeepsRunes(t *testing.T) {
	for limit := 3; limit < 40; limit++ {
		got := clipDetail(strings.Repeat("中a", 30), limit)
		if len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("clip %d produced %d bytes %q", limit, len(got), got)
		}
	}
	if got := clipDetail("short", 10); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
}
