package transferstore

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
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

// StorageErrorSuggestsPathStyle reports whether a failed bucket-level request
// of a virtual-hosted connection is likely an addressing problem: the bucket
// host name did not resolve, or the server dropped the request for an
// unknown host name.
func StorageErrorSuggestsPathStyle(kind StorageErrorKind, pathStyle bool) bool {
	return !pathStyle && (kind == StorageErrorDNS || kind == StorageErrorEOF)
}
