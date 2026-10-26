package transferstore

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const s3ErrorPrefixLimit = 4096

// Observe the SDK's normal response reads. Do not pre-read an error body or
// replace a status/error: the SDK still owns retries, decoding and body closure.
type s3DiagnosticClient struct {
	next        s3.HTTPClient
	credentials []string
}

func (c *s3DiagnosticClient) Do(request *http.Request) (*http.Response, error) {
	var connection struct {
		sync.Mutex
		known, reused bool
		idle          time.Duration
	}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		connection.Lock()
		connection.known, connection.reused, connection.idle = true, info.Reused, info.IdleTime
		connection.Unlock()
	}}
	response, err := c.next.Do(request.WithContext(httptrace.WithClientTrace(request.Context(), trace)))
	if err != nil || response == nil || response.StatusCode < 400 || response.Body == nil {
		return response, err
	}
	fields := []string{fmt.Sprintf("http=%d", response.StatusCode)}
	if value := diagnosticToken(response.Header.Get("Server"), 80, c.credentials); value != "" {
		fields = append(fields, "server="+value)
	}
	if value := response.Header.Get("Content-Type"); len(value) < 256 {
		if mediaType, _, parseErr := mime.ParseMediaType(value); parseErr == nil {
			if value = diagnosticToken(mediaType, 80, c.credentials); value != "" {
				fields = append(fields, "content-type="+value)
			}
		}
	}
	for _, header := range []string{"X-Amz-Request-Id", "X-Oss-Request-Id", "X-Jdcloud-Request-Id"} {
		if value := diagnosticToken(response.Header.Get(header), 256, c.credentials); value != "" {
			fields = append(fields, "request-id="+value)
			break
		}
	}
	switch response.Proto {
	case "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/3.0":
		fields = append(fields, "protocol="+response.Proto)
	}
	connection.Lock()
	if connection.known {
		if connection.reused {
			fields = append(fields, "connection=reused", fmt.Sprintf("idle-ms=%d", connection.idle.Milliseconds()))
		} else {
			fields = append(fields, "connection=new")
		}
	}
	connection.Unlock()
	fields = append(fields, fmt.Sprintf("request-length=%d", request.ContentLength))
	if strings.EqualFold(request.Header.Get("Expect"), "100-continue") {
		fields = append(fields, "expect=100-continue")
	}
	response.Body = &s3ErrorBody{ReadCloser: response.Body, fields: fields}
	return response, nil
}

type s3ErrorBody struct {
	io.ReadCloser
	mu     sync.Mutex
	prefix []byte
	fields []string
}

func (b *s3ErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		if remaining := s3ErrorPrefixLimit - len(b.prefix); remaining > 0 {
			b.prefix = append(b.prefix, p[:min(n, remaining)]...)
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *s3ErrorBody) diagnostic() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.fields, ", ") + ", body=" + s3ErrorBodyKind(b.prefix)
}

// Never copy arbitrary response text. Gateways can echo request URLs, tokens,
// credentials or object contents in their error pages.
func s3ErrorBodyKind(prefix []byte) string {
	text := strings.ToLower(strings.TrimSpace(string(prefix)))
	if strings.HasPrefix(text, "<!doctype html") || strings.HasPrefix(text, "<html") {
		for _, kind := range []struct{ text, name string }{
			{"request header or cookie too large", "html/request-header-too-large"},
			{"request header too large", "html/request-header-too-large"},
			{"request entity too large", "html/request-entity-too-large"},
			{"request timeout", "html/request-timeout"},
			{"the plain http request was sent to https port", "html/http-to-https-port"},
			{"400 bad request", "html/bad-request"},
		} {
			if strings.Contains(text, kind.text) {
				return kind.name
			}
		}
		return "html"
	}
	if strings.HasPrefix(text, "<?xml") || strings.HasPrefix(text, "<error") {
		return "xml"
	}
	if len(prefix) == 0 {
		return "empty"
	}
	return "other"
}

func diagnosticToken(value string, limit int, credentials []string) string {
	if value == "" || len(value) > limit {
		return ""
	}
	for _, secret := range credentials {
		if secret != "" && strings.Contains(value, secret) {
			return ""
		}
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/+-=", r)) {
			return ""
		}
	}
	return value
}

func withS3ResponseDiagnostics(err error) error {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) && responseError.Response != nil && responseError.Response.Response != nil {
		if body, ok := responseError.Response.Body.(*s3ErrorBody); ok {
			return fmt.Errorf("%w [storage response: %s]", err, body.diagnostic())
		}
	}
	return err
}
