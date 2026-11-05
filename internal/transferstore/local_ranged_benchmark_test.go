package transferstore

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pacedWriter caps one HTTP response at bytesPerSecond, the way an object
// store or a long network path caps a single connection.
type pacedWriter struct {
	http.ResponseWriter
	bytesPerSecond int64
	started        time.Time
	sent           int64
}

func (w *pacedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), 64<<10)]
		n, err := w.ResponseWriter.Write(chunk)
		written += n
		w.sent += int64(n)
		if err != nil {
			return written, err
		}
		p = p[n:]
		if due := w.started.Add(time.Duration(w.sent * int64(time.Second) / w.bytesPerSecond)); time.Until(due) > 0 {
			time.Sleep(time.Until(due))
		}
	}
	return written, nil
}

// benchmarkS3 serves one object with conditional ranges through
// http.ServeContent; a nonzero rate paces every response separately.
func benchmarkS3(b *testing.B, payload []byte, rate int64) *S3 {
	b.Helper()
	modified := time.Unix(1700000000, 0)
	etag := fsETag(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/source") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", etag)
		var writer http.ResponseWriter = w
		if rate > 0 && r.Method == http.MethodGet {
			writer = &pacedWriter{ResponseWriter: w, bytesPerSecond: rate, started: time.Now()}
		}
		http.ServeContent(writer, r, "source", modified, bytes.NewReader(payload))
	}))
	b.Cleanup(server.Close)
	client := server.Client()
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = 64
	s, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, HTTPClient: client})
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkLocalDownload measures one large S3 object written to a Local
// target: one stream as before, ranged with the four base slots, and ranged
// on an idle node that lends up to sixteen slots. The paced variants model a
// per-connection limit; "loopback" has none.
//
//	go test ./internal/transferstore -run '^$' -bench BenchmarkLocalDownload -benchtime 3x
func BenchmarkLocalDownload(b *testing.B) {
	const size = 256 << 20
	payload := randomPayload(1, size)
	for _, rate := range []int64{0, 64 << 20} {
		network := "loopback"
		if rate > 0 {
			network = fmt.Sprintf("paced-%dMiBps-per-connection", rate>>20)
		}
		source := benchmarkS3(b, payload, rate)
		object, err := source.Stat(context.Background(), "source")
		if err != nil {
			b.Fatal(err)
		}
		for _, mode := range []struct {
			name string
			min  int64
			idle int
		}{{"stream", -1, 0}, {"ranged-base4", 0, 0}, {"ranged-idle16", 0, 100}} {
			b.Run(network+"/"+mode.name, func(b *testing.B) {
				root := b.TempDir()
				target, err := NewLocal(root)
				if err != nil {
					b.Fatal(err)
				}
				defer target.Close()
				lender := NewPartLender()
				lender.SetIdle(func() int { return mode.idle })
				if err := target.SetRangedWrites(LocalRangedOptions{MinSize: mode.min, PartLender: lender}); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(size)
				b.ResetTimer()
				for range b.N {
					ctx := context.Background()
					options := WriteOptions{Size: size, Overwrite: true}
					var write func() (WriteResult, error)
					if target.RangedWrite(size) {
						ranges, _, err := source.OpenRanges(ctx, "source", OpenOptions{Expected: &object})
						if err != nil {
							b.Fatal(err)
						}
						options.Ranges = ranges.OpenRange
						write = func() (WriteResult, error) {
							defer ranges.Close()
							return target.Write(ctx, "large.bin", strings.NewReader(""), options)
						}
					} else {
						input, _, err := source.Open(ctx, "source", OpenOptions{Expected: &object})
						if err != nil {
							b.Fatal(err)
						}
						write = func() (WriteResult, error) {
							defer input.Close()
							return target.Write(ctx, "large.bin", input, options)
						}
					}
					if result, err := write(); err != nil || !result.Committed {
						b.Fatal(result, err)
					}
				}
				b.StopTimer()
				if got, err := os.ReadFile(filepath.Join(root, "large.bin")); err != nil || !bytes.Equal(got, payload) {
					b.Fatal("downloaded content differs", err)
				}
			})
		}
	}
}
