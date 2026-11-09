package comparison

// Regression tests for the review of the concurrent engine.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type engineIgnoringStartAfter struct{ *engineStore }

func (s engineIgnoringStartAfter) List(ctx context.Context, options transferstore.ListOptions) (transferstore.Page, error) {
	options.StartAfter = ""
	return s.engineStore.List(ctx, options)
}

func TestEngineHeadsOfOneKeyRunConcurrently(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a", "a")
	target.put("backup/a", "a")
	targetStarted := make(chan struct{})
	var once sync.Once
	var overlapped atomic.Bool
	target.statHook = func(context.Context, string) error {
		once.Do(func() { close(targetStarted) })
		return nil
	}
	source.statHook = func(context.Context, string) error {
		select {
		case <-targetStarted:
			overlapped.Store(true)
		case <-time.After(engineHangGuard):
		}
		return nil
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || !overlapped.Load() {
		t.Fatalf("heads did not overlap: %v %+v overlapped=%v", err, rows, overlapped.Load())
	}
}

func TestEngineRangeThatLostOnlyItsEndIsNotAnError(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	data := enginePayload(300<<10, 1)
	source, target := engineRangedPair(data)
	source.rangeHook = func(_ context.Context, _ string, data []byte, offset, length int64, attempt int) (io.ReadCloser, bool, error) {
		if offset == 128<<10 && attempt == 1 {
			return &engineBrokenBody{data: slices.Clone(data[offset : offset+length]), err: io.ErrUnexpectedEOF}, true, nil
		}
		return nil, false, nil
	}
	var read atomic.Int64
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{OnBytes: func(n int64) { read.Add(n) }})
	want := engineSHA256(data)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || rows[0].SourceSHA256 != want || read.Load() != 2*int64(len(data)) {
		t.Fatalf("range body that lost its end (fake store): %v %+v read=%d", err, rows, read.Load())
	}

	data = enginePayload(300<<10, 5)
	fixture := newEngineS3(t)
	sum := md5.Sum(data)
	object := engineS3Object{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`}
	fixture.putObject("source-bucket/src/large", object)
	fixture.putObject("target-bucket/backup/large", object)
	var cut atomic.Int64
	fixture.fault = func(w http.ResponseWriter, r *http.Request, operation, name string, _ int) bool {
		if operation != http.MethodGet || name != "source-bucket/src/large" || r.Header.Get("Range") != "bytes=131072-196607" || !cut.CompareAndSwap(0, 1) {
			return false
		}
		w.Header().Set("ETag", object.etag)
		w.Header().Set("Last-Modified", fixture.headTime)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 131072-196607/%d", len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[131072:196608])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	r := request()
	r.Mode = "content"
	rows, err = engineRun(context.Background(), r, fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{})
	want = engineSHA256(data)
	if cut.Load() != 1 || err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || rows[0].SourceSHA256 != want {
		t.Fatalf("range body that lost its end (S3): %v %+v", err, rows)
	}
}

func TestEngineResumesAfterAStartKeyLongerThanTheListLimit(t *testing.T) {
	engineFastRetries(t)
	fixture := newEngineS3(t)
	for _, long := range []string{strings.Repeat("k", 1015), strings.Repeat("k", 1010) + "日本語"} {
		fixture.put("source-bucket/"+long, "x")
		fixture.put("source-bucket/z", "z")
		fixture.put("target-bucket/archive/2024/z", "z")
		// A target key that sorts between the shortened and the full start key.
		if strings.HasSuffix(long, "語") {
			fixture.put("target-bucket/archive/2024/"+strings.Repeat("k", 1010)+"日", "y")
		}
		r := request()
		r.SourcePrefix, r.DestinationPrefix = "", "archive/2024/"
		rows, err := engineRun(context.Background(), r, fixture.store(t, "source-bucket"), fixture.store(t, "target-bucket"), nil, RunOptions{StartAfter: long})
		var got []string
		for _, row := range rows {
			got = append(got, row.Key+"="+row.Status)
		}
		if err != nil || !slices.Equal(got, []string{"z=" + StatusMatch}) {
			t.Fatalf("resume after a %d-byte start key: %v %v", len(long), err, got)
		}
	}
}

func TestEngineResumeSkipsComparedKeysWhenStorageIgnoresStartAfter(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	for _, key := range []string{"a", "b", "c", "d"} {
		source.put("src/"+key, key)
		target.put("backup/"+key, key)
	}
	rows, err := engineRun(context.Background(), request(), engineIgnoringStartAfter{source}, engineIgnoringStartAfter{target}, nil, RunOptions{StartAfter: "b"})
	var got []string
	for _, row := range rows {
		got = append(got, row.Key+"="+row.Status)
	}
	if err != nil || !slices.Equal(got, []string{"c=" + StatusMatch, "d=" + StatusMatch}) {
		t.Fatalf("storage that ignores start-after: %v %v", err, got)
	}
}
