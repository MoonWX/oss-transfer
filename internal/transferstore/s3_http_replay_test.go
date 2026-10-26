package transferstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type replayRoundTripper func(*http.Request) (*http.Response, error)

func (f replayRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func replayTestStore(t *testing.T, client *http.Client, attempts int) *S3 {
	t.Helper()
	store, err := NewS3(S3Config{Endpoint: "https://storage.invalid", Bucket: "bucket", AccessKey: "access", SecretKey: "secret", PathStyle: true, HTTPClient: client, RetryMaxAttempts: attempts})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3ReplayBodiesPreserveOffsetsAcrossTransportAndSDKRetries(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, operation := range []string{"PutObject", "UploadPart"} {
			t.Run(fmt.Sprintf("%s/disk=%v", operation, disk), func(t *testing.T) {
				payload := bytes.Repeat([]byte("upload-payload/"), 8192)
				prefix := []byte("not-part-of-the-upload/")
				data := append(append([]byte{}, prefix...), payload...)
				var stream io.ReadSeeker = bytes.NewReader(data)
				if disk {
					file, err := os.CreateTemp(t.TempDir(), "upload-")
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					if _, err := file.Write(data); err != nil {
						t.Fatal(err)
					}
					stream = file
				}
				if _, err := stream.Seek(int64(len(prefix)), io.SeekStart); err != nil {
					t.Fatal(err)
				}
				attempts := 0
				var lateBodies []io.ReadCloser
				var lateGetBodies []func() (io.ReadCloser, error)
				transport := replayRoundTripper(func(request *http.Request) (*http.Response, error) {
					attempts++
					if request.GetBody == nil || request.ContentLength != int64(len(payload)) {
						t.Fatalf("attempt %d: missing replay or wrong length %d", attempts, request.ContentLength)
					}
					partial := make([]byte, 7)
					if _, err := io.ReadFull(request.Body, partial); err != nil || !bytes.Equal(partial, payload[:7]) {
						t.Fatalf("attempt %d started at wrong offset: %q, %v", attempts, partial, err)
					}
					request.Body.Close()
					// Concurrent GetBody calls must each begin at the upload start,
					// including after the transport closed the original request.
					var readers sync.WaitGroup
					for range 3 {
						readers.Go(func() {
							body, err := request.GetBody()
							if err != nil {
								t.Error(err)
								return
							}
							got, err := io.ReadAll(body)
							if err != nil || !bytes.Equal(got, payload) {
								t.Errorf("replay bytes=%d error=%v", len(got), err)
							}
							body.Close()
							if n, err := body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
								t.Errorf("closed body can still read: %d, %v", n, err)
							}
						})
					}
					readers.Wait()
					lateBody, err := request.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					lateBodies = append(lateBodies, lateBody)
					lateGetBodies = append(lateGetBodies, request.GetBody)
					if offset, err := stream.Seek(0, io.SeekCurrent); err != nil || offset != int64(len(prefix)+7) {
						t.Fatalf("replay moved the source cursor: %d, %v", offset, err)
					}
					status, response := 200, ""
					if attempts == 1 {
						status, response = 503, "<Error><Code>SlowDown</Code></Error>"
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Etag": {`"etag"`}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
				})
				store := replayTestStore(t, &http.Client{Transport: transport}, 2)
				options := func(o *s3.Options) {
					o.Retryer = retry.NewStandard(func(r *retry.StandardOptions) {
						r.MaxAttempts = 2
						r.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
					})
				}
				var err error
				if operation == "PutObject" {
					_, err = store.client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("object"), Body: stream, ContentLength: aws.Int64(int64(len(payload)))}, options)
				} else {
					_, err = store.client.UploadPart(context.Background(), &s3.UploadPartInput{Bucket: aws.String("bucket"), Key: aws.String("object"), UploadId: aws.String("upload-id"), PartNumber: aws.Int32(1), Body: stream, ContentLength: aws.Int64(int64(len(payload)))}, options)
				}
				if err != nil || attempts != 2 {
					t.Fatalf("attempts=%d error=%v", attempts, err)
				}
				for i, body := range lateBodies {
					if n, err := body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
						t.Errorf("request returned with readable replay: %d, %v", n, err)
					}
					if _, err := lateGetBodies[i](); !errors.Is(err, io.ErrClosedPipe) {
						t.Errorf("replay retained source after request returned: %v", err)
					}
				}
			})
		}
	}
}

type replayReadSeekerOnly struct{ io.ReadSeeker }

func TestS3ReplayDoesNotBufferOtherStreamsOrReplayPost(t *testing.T) {
	var calls int
	transport := replayRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.GetBody != nil {
			t.Errorf("unexpected replay for %s", request.Method)
		}
		response := ""
		if request.Method == http.MethodPut {
			got, err := io.ReadAll(request.Body)
			if err != nil || string(got) != "payload" {
				t.Errorf("body changed: %q, %v", got, err)
			}
			request.Body.Close()
		} else {
			response = "<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	store := replayTestStore(t, &http.Client{Transport: transport}, 1)
	_, err := store.client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key"), Body: replayReadSeekerOnly{strings.NewReader("payload")}, ContentLength: aws.Int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

type replayCountReader struct {
	io.Reader
	read int
}

func (r *replayCountReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

// A real TLS HTTP/2 peer declines the first stream with GOAWAY only after it
// received DATA. net/http must use GetBody to replay on a second connection.
// SDK attempts are limited to one, so SDK retries cannot make this test pass.
func TestS3HTTP2GoAwayReplaysBodyWithinOneSDKAttempt(t *testing.T) {
	for _, test := range []struct {
		name               string
		rejected, disabled bool
	}{
		{name: "replayed"},
		{name: "replayed-but-rejected", rejected: true},
		{name: "without-fix", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("replayed-upload/"), 32768)
			var connections, firstBytes atomic.Int32
			var got []byte
			var gotMu sync.Mutex
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("expected HTTP/2")
			}))
			server.EnableHTTP2 = true
			server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				connection := connections.Add(1)
				preface := make([]byte, 24)
				if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" {
					t.Errorf("HTTP/2 preface: %q, %v", preface, err)
					return
				}
				// Increase the stream and connection windows for the full body.
				replayWriteH2Frame(t, conn, 4, 0, 0, []byte{0, 4, 0, 16, 0, 0})
				replayWriteH2Frame(t, conn, 8, 0, 0, []byte{0, 16, 0, 0})
				var uploaded []byte
				for {
					kind, flags, stream, body, err := replayReadH2Frame(conn)
					if err != nil {
						t.Errorf("HTTP/2 frame on connection %d: %v", connection, err)
						return
					}
					if kind == 4 && flags&1 == 0 {
						replayWriteH2Frame(t, conn, 4, 1, 0, nil)
					}
					if kind != 0 {
						continue
					}
					if connection == 1 && len(body) > 0 {
						firstBytes.Add(int32(len(body)))
						// NO_ERROR, last processed stream = 0: the server did not
						// accept this upload, even though body bytes were sent.
						replayWriteH2Frame(t, conn, 7, 0, 0, make([]byte, 8))
						// Keep the old socket readable until the client handles
						// GOAWAY; closing it now could race into a broken pipe.
						io.Copy(io.Discard, conn)
						return
					}
					uploaded = append(uploaded, body...)
					if flags&1 != 0 {
						gotMu.Lock()
						got = uploaded
						gotMu.Unlock()
						status := byte(0x88) // HPACK static table: :status 200.
						if test.rejected {
							status = 0x8c // :status 400.
						}
						replayWriteH2Frame(t, conn, 1, 5, stream, []byte{status})
						return
					}
				}
			}}
			server.StartTLS()
			defer server.Close()
			defer server.Client().CloseIdleConnections()
			store, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "access", SecretKey: "secret", PathStyle: true, HTTPClient: server.Client(), RetryMaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			if test.disabled {
				store.client = s3.New(store.client.Options(), func(o *s3.Options) { o.APIOptions = nil })
			}
			source := &replayCountReader{Reader: bytes.NewReader(payload)}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result, err := store.Write(ctx, "key", source, WriteOptions{Size: int64(len(payload)), Overwrite: true})
			if test.disabled {
				if err == nil || result.Committed || !strings.Contains(err.Error(), "define Request.GetBody") || connections.Load() != 1 || firstBytes.Load() <= 0 {
					t.Fatalf("expected original GOAWAY replay failure: connections=%d first-bytes=%d result=%+v error=%v", connections.Load(), firstBytes.Load(), result, err)
				}
				return
			}
			if test.rejected && (err == nil || result.Committed) || !test.rejected && (err != nil || !result.Committed) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			gotMu.Lock()
			defer gotMu.Unlock()
			if connections.Load() != 2 || firstBytes.Load() <= 0 || !bytes.Equal(got, payload) || source.read != len(payload) {
				t.Fatalf("connections=%d first-bytes=%d replayed-bytes=%d source-bytes=%d", connections.Load(), firstBytes.Load(), len(got), source.read)
			}
		})
	}
}

func replayWriteH2Frame(t *testing.T, conn net.Conn, kind, flags byte, stream uint32, payload []byte) {
	t.Helper()
	header := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), kind, flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[5:], stream)
	if _, err := conn.Write(append(header, payload...)); err != nil {
		t.Errorf("write HTTP/2 frame: %v", err)
	}
}

func replayReadH2Frame(conn net.Conn) (kind, flags byte, stream uint32, payload []byte, err error) {
	header := make([]byte, 9)
	if _, err = io.ReadFull(conn, header); err != nil {
		return
	}
	size := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	if size > 1<<20 {
		err = fmt.Errorf("HTTP/2 frame too large: %d", size)
		return
	}
	kind, flags, stream = header[3], header[4], binary.BigEndian.Uint32(header[5:])&0x7fffffff
	payload = make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	return
}
