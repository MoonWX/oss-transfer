package transferstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type countingHEADTransport struct {
	base  http.RoundTripper
	heads atomic.Int64
}

func (c *countingHEADTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodHead {
		c.heads.Add(1)
	}
	return c.base.RoundTrip(r)
}

func TestS3PrecheckedMissingDestinationStillConditional(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "stale_hint"}[existing], func(t *testing.T) {
			p := newProtocolS3(t)
			transport := &countingHEADTransport{base: p.server.Client().Transport}
			if transport.base == nil {
				transport.base = http.DefaultTransport
			}
			s, err := NewS3(S3Config{Endpoint: p.server.URL, Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("new payload")
			prior := []byte("must survive stale hint")
			if existing {
				p.objects["key"] = protocolObject{data: prior, etag: objectETag(prior)}
			}
			result, err := s.Write(context.Background(), "key", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), DestinationAbsent: true})
			if err != nil {
				t.Fatal(err)
			}
			if !p.conditional {
				t.Fatal("missing conditional write")
			}
			if existing {
				if !result.Skipped || result.Committed || transport.heads.Load() != 1 || !bytes.Equal(p.objects["key"].data, prior) {
					t.Fatalf("stale hint overwrote object: %+v", result)
				}
			} else {
				if !result.Committed || result.Skipped || transport.heads.Load() != 0 || sha256.Sum256(p.objects["key"].data) != sha256.Sum256(payload) {
					t.Fatalf("redundant HEAD or content mismatch: %+v", result)
				}
			}
		})
	}
}

func TestS3PrecheckedDestinationConditionalUnsupportedFailsClosed(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(map[bool]string{false: "put", true: "multipart"}[multipart], func(t *testing.T) {
			p := newProtocolS3(t)
			var publications atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.Method == "PUT" && r.URL.Query().Get("partNumber") == "") || (r.Method == "POST" && r.URL.Query().Get("uploadId") != "") {
					publications.Add(1)
					if r.Header.Get("If-None-Match") != "*" {
						t.Error("unconditional fallback attempted")
					}
					protocolError(w, 501, "NotImplemented")
					return
				}
				p.serve(w, r)
			}))
			defer server.Close()
			s, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, RetryMaxAttempts: 1, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			size := int64(10)
			if multipart {
				size = minPartSize + 1
			}
			payload := bytes.Repeat([]byte("p"), int(size))
			result, err := s.Write(context.Background(), "key", bytes.NewReader(payload), WriteOptions{Size: size, DestinationAbsent: true})
			if err == nil || result.Committed || result.Skipped || publications.Load() != 1 {
				t.Fatalf("conditional failure accepted: %+v %v attempts=%d", result, err, publications.Load())
			}
			if len(p.objects) != 0 || p.completed != 0 || (multipart && p.aborted != 1) {
				t.Fatal("conditional rejection wrote target or leaked upload")
			}
		})
	}
}
