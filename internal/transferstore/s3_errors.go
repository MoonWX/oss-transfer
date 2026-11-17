package transferstore

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// StorageErrorKind names a common storage failure that users can act on. The
// values are stable API identifiers.
type StorageErrorKind string

const (
	StorageErrorDNS               StorageErrorKind = "dns"
	StorageErrorConnectionRefused StorageErrorKind = "connection_refused"
	StorageErrorConnectionReset   StorageErrorKind = "connection_reset"
	StorageErrorTimeout           StorageErrorKind = "timeout"
	StorageErrorTLS               StorageErrorKind = "tls"
	StorageErrorAccessDenied      StorageErrorKind = "access_denied"
	StorageErrorNoSuchBucket      StorageErrorKind = "no_such_bucket"
	StorageErrorInvalidAccessKey  StorageErrorKind = "invalid_access_key"
	StorageErrorSignatureMismatch StorageErrorKind = "signature_mismatch"
	StorageErrorEOF               StorageErrorKind = "eof"
	StorageErrorUnknown           StorageErrorKind = ""
)

var storageErrorCodes = map[string]StorageErrorKind{
	"AccessDenied":          StorageErrorAccessDenied,
	"AllAccessDisabled":     StorageErrorAccessDenied,
	"NoSuchBucket":          StorageErrorNoSuchBucket,
	"InvalidAccessKeyId":    StorageErrorInvalidAccessKey,
	"SignatureDoesNotMatch": StorageErrorSignatureMismatch,
}

// IsAccessDenied reports whether the storage refused a request (HTTP 403).
// A HEAD response has no body, so its error code is only "Forbidden"; the
// status decides.
func IsAccessDenied(err error) bool {
	var response *awshttp.ResponseError
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusForbidden {
		return true
	}
	return ClassifyStorageError(err) == StorageErrorAccessDenied
}

// ClassifyStorageError returns the kind of a storage error, or
// StorageErrorUnknown. Typed errors are preferred; the text is matched too, so
// a message relayed from another process (an Agent reply, a stored task
// failure) classifies the same way.
func ClassifyStorageError(err error) StorageErrorKind {
	if err == nil {
		return StorageErrorUnknown
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		if kind, ok := storageErrorCodes[apiError.ErrorCode()]; ok {
			return kind
		}
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		if dnsError.IsTimeout {
			return StorageErrorTimeout
		}
		return StorageErrorDNS
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return StorageErrorTLS
	}
	if kind := ClassifyStorageMessage(err.Error()); kind != StorageErrorUnknown {
		return kind
	}
	var netError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netError) && netError.Timeout() {
		return StorageErrorTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return StorageErrorEOF
	}
	return StorageErrorUnknown
}

// modeledErrorCode finds the code of an error the SDK models (NoSuchBucket),
// which it reports as "<code>: <message>" after the response's request IDs,
// without the "api error" prefix of other codes.
var modeledErrorCode = regexp.MustCompile(`HostID: [^,]*, (\w+): `)

var (
	apiErrorCode       = regexp.MustCompile(`api error (\w+)`)
	xmlErrorCode       = regexp.MustCompile(`<Code>(\w+)</Code>`)
	responseStatusCode = regexp.MustCompile(`StatusCode: (\d{3})\b`)
)

// StorageErrorReply returns the error code and HTTP status of the storage
// reply that an error's text reports, as the SDK formats it. A reply without
// a body (HEAD, or an error page) has only its status text as code, such as
// "Forbidden"; that code is returned empty, so the status decides. The code is
// empty and the status 0 when the text reports no reply, as for a connection
// that failed.
func StorageErrorReply(message string) (code string, status int) {
	if match := responseStatusCode.FindStringSubmatch(message); match != nil {
		status, _ = strconv.Atoi(match[1])
	}
	for _, pattern := range []*regexp.Regexp{apiErrorCode, modeledErrorCode, xmlErrorCode} {
		if match := pattern.FindStringSubmatch(message); match != nil {
			code = match[1]
			break
		}
	}
	if status > 0 && code == strings.ReplaceAll(http.StatusText(status), " ", "") {
		code = ""
	}
	return code, status
}

// IsThrottleCode reports whether a storage error code asks the client to
// lower its request rate (see IsThrottleResponse).
func IsThrottleCode(code string) bool { return throttleErrorCodes[code] }

// ClassifyStorageMessage classifies the text of a storage error. The order
// matters: a storage error code in the reply wins over the transport words
// that may surround it, and a TLS handshake timeout is a timeout.
func ClassifyStorageMessage(message string) StorageErrorKind {
	lower := strings.ToLower(message)
	modeled := ""
	if match := modeledErrorCode.FindStringSubmatch(message); match != nil {
		modeled = match[1]
	}
	for _, code := range []string{"InvalidAccessKeyId", "SignatureDoesNotMatch", "NoSuchBucket", "AccessDenied", "AllAccessDisabled"} {
		if strings.Contains(message, "api error "+code) || strings.Contains(message, "<Code>"+code+"</Code>") || modeled == code {
			return storageErrorCodes[code]
		}
	}
	switch {
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "server misbehaving") && strings.Contains(lower, "lookup "),
		strings.Contains(lower, "temporary failure in name resolution"), strings.Contains(lower, "name or service not known"),
		strings.Contains(lower, "nodename nor servname"):
		return StorageErrorDNS
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "actively refused"):
		return StorageErrorConnectionRefused
	case strings.Contains(lower, "connection reset"), strings.Contains(lower, "forcibly closed"), strings.Contains(lower, "broken pipe"):
		return StorageErrorConnectionReset
	case strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "timeout awaiting"), strings.Contains(lower, "handshake timeout"),
		strings.Contains(lower, "client.timeout exceeded"), strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "connection timed out"), strings.Contains(lower, "operation timed out"):
		return StorageErrorTimeout
	case strings.Contains(lower, "x509:"), strings.Contains(lower, "tls: failed to verify certificate"),
		strings.Contains(lower, "certificate signed by unknown authority"), strings.Contains(lower, "certificate is not valid"),
		strings.Contains(lower, "certificate has expired"):
		return StorageErrorTLS
	case strings.HasSuffix(strings.TrimSpace(message), "EOF"), strings.Contains(message, ": EOF"), strings.Contains(lower, "unexpected eof"):
		return StorageErrorEOF
	}
	return StorageErrorUnknown
}

// endpointMismatch is how S3 (PermanentRedirect, HTTP 301) and Alibaba Cloud
// OSS (AccessDenied, HTTP 403, EC 0003-00001403) say that a bucket must be
// reached through another endpoint, usually the one of its region.
const endpointMismatch = "must be addressed using the specified endpoint"

var (
	diagnosticEndpoint = regexp.MustCompile(`\[storage response: [^\]]*\bendpoint=([A-Za-z0-9._-]+)`)
	// An OSS reply without a body, such as that to HEAD, says it by its EC
	// header alone (see the storage response diagnostic).
	diagnosticEndpointCode = regexp.MustCompile(`\[storage response: [^\]]*\bec=0003-00001403\b`)
)

// EndpointMismatch reports whether a storage error says that the bucket must
// be addressed through another endpoint, and that endpoint when the reply
// named it (see the storage response diagnostic). OSS answers such a request
// with AccessDenied although the credentials are not the problem.
func EndpointMismatch(message string) (bool, string) {
	if !strings.Contains(strings.ToLower(message), endpointMismatch) && !diagnosticEndpointCode.MatchString(message) {
		return false, ""
	}
	if match := diagnosticEndpoint.FindStringSubmatch(message); match != nil {
		return true, match[1]
	}
	return true, ""
}

// StorageErrorSuggestsPathStyle reports whether a failed bucket-level request
// of a virtual-hosted connection is likely an addressing problem: the bucket
// host name did not resolve, or the server dropped the request for an
// unknown host name.
func StorageErrorSuggestsPathStyle(kind StorageErrorKind, pathStyle bool) bool {
	return !pathStyle && (kind == StorageErrorDNS || kind == StorageErrorEOF)
}
