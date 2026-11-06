package transferstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fuseLikeFile models a FUSE network filesystem with kernel writeback: data
// written at the end of the previous write streams at sequentialRate, while a
// write at any other offset first pays seekPenalty, as when the writeback
// cache must flush before the gap. The default penalty makes 256 KiB scattered
// writes run at about 55 MiB/s, the rate observed on the jdufs test mount.
type fuseLikeFile struct {
	mu             sync.Mutex
	end, size      int64
	sequentialRate int64
	seekPenalty    time.Duration
	debt           time.Duration
}

func newFuseLikeFile() *fuseLikeFile {
	return &fuseLikeFile{sequentialRate: 1 << 30, seekPenalty: 4 * time.Millisecond}
}

func (f *fuseLikeFile) WriteAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cost := time.Duration(int64(len(p)) * int64(time.Second) / f.sequentialRate)
	if offset != f.end {
		cost += f.seekPenalty
	}
	// Sleep in batches of at least a millisecond, so timer resolution does
	// not inflate many small sequential writes.
	if f.debt += cost; f.debt >= time.Millisecond {
		time.Sleep(f.debt)
		f.debt = 0
	}
	f.end = offset + int64(len(p))
	f.size = max(f.size, f.end)
	return len(p), nil
}

func (f *fuseLikeFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	offset := f.end
	f.mu.Unlock()
	return f.WriteAt(p, offset)
}

// scatteredRanges is the previous (v4.1.30 branch) local strategy, kept only
// for comparison: every slot streams its range straight to its own offset.
func scatteredRanges(ctx context.Context, w io.WriterAt, ranges RangeFunc, size, part int64, slots int) error {
	var group sync.WaitGroup
	var failure sync.Once
	var first error
	limit := make(chan struct{}, slots)
	for offset := int64(0); offset < size; offset += part {
		limit <- struct{}{}
		group.Add(1)
		go func(offset, length int64) {
			defer group.Done()
			defer func() { <-limit }()
			input, err := ranges(ctx, offset, length)
			if err == nil {
				buffer := make([]byte, localCopyBuffer)
				for written := int64(0); written < length && err == nil; {
					var n int
					n, err = input.Read(buffer[:min(int64(len(buffer)), length-written)])
					if n > 0 {
						_, _ = w.WriteAt(buffer[:n], offset+written)
						written += int64(n)
					}
					if err == io.EOF && written == length {
						err = nil
					}
				}
				_ = input.Close()
			}
			if err != nil {
				failure.Do(func() { first = err })
			}
		}(offset, min(part, size-offset))
	}
	group.Wait()
	return first
}

// BenchmarkLocalDownload measures one 256 MiB S3 object written locally:
//
//   - stream:            one GET, sequential writes (ranged downloads off)
//   - ordered-base4/16:  this design, 8 MiB chunks written in order, with the
//     four guaranteed slots or an idle node lending up to sixteen
//   - scattered-16:      the replaced design, 16 MiB ranges in 16 slots, each
//     writing at its own offset
//
// against a real file ("disk") and the FUSE model above ("fuse"), over an
// unpaced loopback and with every connection paced to 64 MiB/s.
//
//	go test ./internal/transferstore -run '^$' -bench BenchmarkLocalDownload -benchtime 3x
func BenchmarkLocalDownload(b *testing.B) {
	const size = 256 << 20
	payload := randomPayload(1, size)
	for _, rate := range []int64{0, 64 << 20} {
		network := "loopback"
		if rate > 0 {
			network = fmt.Sprintf("paced%dMiBps", rate>>20)
		}
		source := benchmarkS3(b, payload, rate)
		object, err := source.Stat(context.Background(), "source")
		if err != nil {
			b.Fatal(err)
		}
		for _, filesystem := range []string{"disk", "fuse"} {
			for _, mode := range []string{"stream", "ordered-base4", "ordered-idle16", "scattered-16"} {
				b.Run(network+"/"+filesystem+"/"+mode, func(b *testing.B) {
					benchmarkDownload(b, source, object, payload, filesystem, mode)
				})
			}
		}
	}
}

func benchmarkDownload(b *testing.B, source *S3, object Object, payload []byte, filesystem, mode string) {
	size := int64(len(payload))
	root := b.TempDir()
	target, err := NewLocal(root)
	if err != nil {
		b.Fatal(err)
	}
	defer target.Close()
	budget, _ := NewUploadMemoryBudget(DefaultUploadMemoryBytes)
	lender := NewPartLender()
	idle := 0
	if mode == "ordered-idle16" {
		idle = 100
	}
	lender.SetIdle(func() int { return idle })
	if err := target.SetRangedWrites(LocalRangedOptions{PartLender: lender, Memory: budget}); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		var file interface {
			io.Writer
			io.WriterAt
		}
		var real *os.File
		if filesystem == "fuse" {
			file = newFuseLikeFile()
		} else {
			real, err = os.Create(filepath.Join(root, "large.bin"))
			if err != nil {
				b.Fatal(err)
			}
			file = real
		}
		switch mode {
		case "stream":
			input, _, err := source.Open(ctx, "source", OpenOptions{Expected: &object})
			if err == nil {
				_, err = io.CopyBuffer(file, io.LimitReader(input, size), make([]byte, localCopyBuffer))
				_ = input.Close()
			}
			if err != nil {
				b.Fatal(err)
			}
		default:
			ranges, _, err := source.OpenRanges(ctx, "source", OpenOptions{Expected: &object})
			if err != nil {
				b.Fatal(err)
			}
			if mode == "scattered-16" {
				if real != nil {
					_ = real.Truncate(size)
				}
				err = scatteredRanges(ctx, file, ranges.OpenRange, size, 16<<20, 16)
			} else {
				err = target.writeRanges(ctx, file, ranges.OpenRange, size, nil)
			}
			_ = ranges.Close()
			if err != nil {
				b.Fatal(err)
			}
		}
		if real != nil {
			if err := real.Sync(); err != nil {
				b.Fatal(err)
			}
			_ = real.Close()
		}
	}
	b.StopTimer()
	if filesystem == "disk" {
		if got, err := os.ReadFile(filepath.Join(root, "large.bin")); err != nil || !bytes.Equal(got, payload) {
			b.Fatal("downloaded content differs", err)
		}
	}
}
