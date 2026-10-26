package transferstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func TestS3HTTP400DiagnosticsRetainSDKFailureWithoutRetryOrSecrets(t *testing.T) {
	var uploads atomic.Int32
	const payload = "private-object-contents"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		uploads.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != payload || r.ContentLength != int64(len(payload)) {
			t.Errorf("request body changed: %q, %v, length=%d", data, err, r.ContentLength)
		}
		w.Header().Set("Server", "nginx/1.24.0")
		w.Header().Set("Content-Type", "text/html; marker=private-parameter")
		w.Header().Set("X-Amz-Request-Id", "trace-123")
		w.Header().Set("Set-Cookie", "session=private-cookie")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<html><head><title>400 Bad Request</title></head><body>private-parameter private-cookie test-access test-secret test-session `+payload+` https://secret.example/object?uploadId=secret-id<hr></body></html>`)
	}))
	defer server.Close()
	client := server.Client()
	response, err := client.Get(server.URL + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	s, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "test-secret", SessionToken: "test-session", PathStyle: true, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Write(context.Background(), "key", strings.NewReader(payload), WriteOptions{Size: int64(len(payload)), Overwrite: true})
	if err == nil || result.Committed || uploads.Load() != 1 {
		t.Fatalf("400 changed to success or retried: result=%+v attempts=%d error=%v", result, uploads.Load(), err)
	}
	var responseError *smithyhttp.ResponseError
	if !errors.As(err, &responseError) || responseError.HTTPStatusCode() != 400 {
		t.Fatalf("lost SDK response error: %v", err)
	}
	for _, want := range []string{"XML syntax error", "server=nginx/1.24.0", "content-type=text/html", "request-id=trace-123", "connection=reused", "body=html/bad-request"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	for _, secret := range []string{"private-parameter", "private-cookie", "test-access", "test-secret", "test-session", payload, "secret.example", "uploadId", "secret-id"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("response diagnostic exposed %q: %v", secret, err)
		}
	}
}

type diagnosticHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f diagnosticHTTPClientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type diagnosticTestBody struct {
	reader        io.Reader
	reads, closes int
}

func (b *diagnosticTestBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *diagnosticTestBody) Close() error { b.closes++; return nil }

func TestS3HTTPDiagnosticsDoNotPreReadOrConsumeResponseAndStayBounded(t *testing.T) {
	payload := bytes.Repeat([]byte("private-body"), 10000)
	body := &diagnosticTestBody{reader: bytes.NewReader(payload)}
	response := &http.Response{StatusCode: 400, Header: http.Header{"Server": {"private-access"}, "X-Amz-Request-Id": {"trace\nAuthorization: private-secret"}}, Body: body}
	client := &s3DiagnosticClient{next: diagnosticHTTPClientFunc(func(*http.Request) (*http.Response, error) { return response, nil }), credentials: []string{"private-access", "private-secret"}}
	request, _ := http.NewRequest(http.MethodPut, "https://example.invalid/secret?token=private-token", nil)
	actual, err := client.Do(request)
	if err != nil || actual != response || body.reads != 0 {
		t.Fatalf("diagnostics read or replaced the response: %v, reads=%d", err, body.reads)
	}
	data, err := io.ReadAll(actual.Body)
	if !bytes.Equal(data, payload) || err != io.ErrUnexpectedEOF {
		t.Fatalf("SDK reads were changed: bytes=%d error=%v", len(data), err)
	}
	actual.Body.Close()
	captured := actual.Body.(*s3ErrorBody)
	if len(captured.prefix) != s3ErrorPrefixLimit || body.closes != 1 {
		t.Fatalf("unbounded capture or wrong close: prefix=%d closes=%d", len(captured.prefix), body.closes)
	}
	if strings.Contains(captured.diagnostic(), "private") || strings.Contains(captured.diagnostic(), "Authorization") {
		t.Fatalf("unsafe diagnostic: %s", captured.diagnostic())
	}
}

func TestS3HTTPDiagnosticsPreserveNotFoundClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocolError(w, http.StatusNotFound, "NoSuchKey")
	}))
	defer server.Close()
	s, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Open(context.Background(), "key", OpenOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("lost source not found classification: %v", err)
	}
}
