package comparison

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func engineContentRequest() Request {
	r := request()
	r.Mode = "content"
	return r
}

// enginePayload is deterministic pseudo-random content.
func enginePayload(size int, seed uint64) []byte {
	random := rand.New(rand.NewPCG(seed, seed+1))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(random.UintN(256))
	}
	return data
}

func engineSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// engineRangedPair stores data under src/obj and backup/obj in two stores that
// serve ranges.
func engineRangedPair(data []byte) (*engineRangeStore, *engineRangeStore) {
	source, target := &engineRangeStore{engineStore: newEngineStore()}, &engineRangeStore{engineStore: newEngineStore()}
	object := engineObject{data: data, etag: engineMD5ETag(data), modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	source.putObject("src/obj", object)
	target.putObject("backup/obj", object)
	return source, target
}

// engineLengthRecorder is a ReaderWrapper that records the lengths it wraps.
type engineLengthRecorder struct {
	mu      sync.Mutex
	lengths []int64
}

func (r *engineLengthRecorder) wrap(_ context.Context, input io.ReadCloser, length int64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lengths = append(r.lengths, length)
	return input, nil
}

// engineRangeLengths returns the sorted lengths of every range requested.
func engineRangeLengths(t *testing.T, stores ...*engineRangeStore) []int64 {
	t.Helper()
	var lengths []int64
	for _, store := range stores {
		store.mu.Lock()
		for _, entry := range store.rangeLog {
			length, err := strconv.ParseInt(entry[strings.LastIndexByte(entry, '+')+1:], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			lengths = append(lengths, length)
		}
		store.mu.Unlock()
	}
	slices.Sort(lengths)
	return lengths
}

func TestEngineContentRangeResumesAfterBrokenStream(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	data := enginePayload(300<<10, 1)
	source, target := engineRangedPair(data)
	source.rangeHook = func(_ context.Context, _ string, data []byte, offset, length int64, attempt int) (io.ReadCloser, bool, error) {
		if offset == 128<<10 && attempt == 1 {
			// The connection breaks after 20,000 bytes of this range.
			return &engineBrokenBody{data: slices.Clone(data[offset : offset+20000]), err: io.ErrUnexpectedEOF}, true, nil
		}
		return nil, false, nil
	}
	var read atomic.Int64
	recorder := &engineLengthRecorder{}
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, recorder.wrap, RunOptions{OnBytes: func(n int64) { read.Add(n) }})
	if err != nil || len(rows) != 1 {
		t.Fatalf("comparison failed: %v %+v", err, rows)
	}
	want := engineSHA256(data)
	if row := rows[0]; row.Status != StatusMatch || row.SourceSHA256 != want || row.DestinationSHA256 != want {
		t.Fatalf("a retried range changed the hash: %+v, want %s", row, want)
	}
	source.mu.Lock()
	log := slices.Clone(source.rangeLog)
	source.mu.Unlock()
	wantLog := []string{"src/obj@0+65536", "src/obj@65536+65536", "src/obj@131072+65536", "src/obj@151072+45536", "src/obj@196608+65536", "src/obj@262144+45056"}
	if !slices.Equal(log, wantLog) {
		t.Fatalf("source ranges = %v, want %v", log, wantLog)
	}
	// Every byte is hashed and reported once, retry or not.
	if read.Load() != 2*int64(len(data)) {
		t.Fatalf("OnBytes reported %d bytes, want %d", read.Load(), 2*len(data))
	}
	recorder.mu.Lock()
	wrapped := slices.Clone(recorder.lengths)
	recorder.mu.Unlock()
	slices.Sort(wrapped)
	if ranges := engineRangeLengths(t, source, target); !slices.Equal(wrapped, ranges) {
		t.Fatalf("wrapped lengths %v do not match the range lengths %v", wrapped, ranges)
	}
}

func TestEngineContentStalledReadsAreRetried(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	// The idle limit only has to outlast one in-memory read; it is not a
	// speed requirement, and a spurious expiry would only cause a retry.
	engineOverride(t, &readIdleTimeout, 300*time.Millisecond)
	data := enginePayload(200<<10, 2)
	source, target := engineRangedPair(data)
	var idleCauses atomic.Int64
	source.rangeHook = func(ctx context.Context, _ string, data []byte, offset, length int64, attempt int) (io.ReadCloser, bool, error) {
		switch {
		case offset == 0 && attempt == 1:
			// Data stops arriving after 1,000 bytes.
			go func() {
				<-ctx.Done()
				if errors.Is(context.Cause(ctx), errComparisonReadIdle) {
					idleCauses.Add(1)
				}
			}()
			return &engineStalledBody{ctx: ctx, data: slices.Clone(data[:1000])}, true, nil
		case offset == 64<<10 && attempt == 1:
			// No response arrives: time to first byte counts as idle.
			<-ctx.Done()
			if errors.Is(context.Cause(ctx), errComparisonReadIdle) {
				idleCauses.Add(1)
			}
			return nil, true, ctx.Err()
		}
		return nil, false, nil
	}
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("comparison failed: %v %+v", err, rows)
	}
	if row, want := rows[0], engineSHA256(data); row.Status != StatusMatch || row.SourceSHA256 != want {
		t.Fatalf("stalled reads were not retried correctly: %+v", row)
	}
	source.mu.Lock()
	log := slices.Clone(source.rangeLog)
	source.mu.Unlock()
	stalledFirstByte := 0
	for _, entry := range log {
		if entry == "src/obj@65536+65536" {
			stalledFirstByte++
		}
	}
	if !slices.Contains(log, "src/obj@1000+64536") || stalledFirstByte < 2 {
		t.Fatalf("stalled attempts were not retried from the right offsets: %v", log)
	}
	deadline := time.Now().Add(engineHangGuard)
	for idleCauses.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d stalled attempts were ended by the idle watchdog, want 2", idleCauses.Load())
		}
		time.Sleep(time.Millisecond)
	}

	// A read that never resumes ends as an error row after its retries.
	stuck, other := engineRangedPair(data)
	stuck.rangeHook = func(ctx context.Context, _ string, data []byte, offset, _ int64, _ int) (io.ReadCloser, bool, error) {
		if offset == 0 {
			return &engineStalledBody{ctx: ctx}, true, nil
		}
		return nil, false, nil
	}
	rows, err = engineRun(context.Background(), engineContentRequest(), stuck, other, nil, RunOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("comparison failed: %v %+v", err, rows)
	}
	if row := rows[0]; row.Status != StatusError || row.Error != "Cannot read complete object content" || row.ErrorDetail != "source GET: timeout" {
		t.Fatalf("stuck read row: %+v", row)
	}
	if attempts := stuck.attempt("range src/obj@0") - 1; attempts != 1+len(rangeRetryDelays) {
		t.Fatalf("stuck range was attempted %d times, want %d", attempts, 1+len(rangeRetryDelays))
	}
}

func TestEngineContentStreamsWhenRangesAreUnavailable(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	data := enginePayload(300<<10, 3)
	source, target := engineRangedPair(data)
	source.unsupported = true
	source.openHook = func(_ context.Context, _ string, data []byte, attempt int) (io.ReadCloser, bool, error) {
		if attempt == 1 {
			return &engineBrokenBody{data: slices.Clone(data[:100000]), err: io.ErrUnexpectedEOF}, true, nil
		}
		return nil, false, nil
	}
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
	want := engineSHA256(data)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || rows[0].SourceSHA256 != want || rows[0].DestinationSHA256 != want {
		t.Fatalf("stream retry produced a wrong result: %v %+v", err, rows)
	}
	if source.opens.Load() != 2 || len(source.rangeLog) != 0 || target.opens.Load() != 0 {
		t.Fatalf("source opened %d times with %d ranges; target opened %d times", source.opens.Load(), len(source.rangeLog), target.opens.Load())
	}

	// A body that keeps breaking ends as an error row after two retries.
	broken, other := engineRangedPair(data)
	broken.unsupported = true
	broken.openHook = func(_ context.Context, _ string, data []byte, _ int) (io.ReadCloser, bool, error) {
		return &engineBrokenBody{data: slices.Clone(data[:10]), err: io.ErrUnexpectedEOF}, true, nil
	}
	rows, err = engineRun(context.Background(), engineContentRequest(), broken, other, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusError || rows[0].ErrorDetail != "source GET: network error" || broken.opens.Load() != int64(1+len(streamRetryDelays)) {
		t.Fatalf("broken stream: %v %+v after %d opens", err, rows, broken.opens.Load())
	}

	// Objects of one range use a single GET even when ranges are available.
	small, smallTarget := engineRangedPair(data[:64<<10])
	rows, err = engineRun(context.Background(), engineContentRequest(), small, smallTarget, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch || small.opens.Load() != 1 || smallTarget.opens.Load() != 1 || len(small.rangeLog)+len(smallTarget.rangeLog) != 0 {
		t.Fatalf("small object: %v %+v, opens %d/%d", err, rows, small.opens.Load(), smallTarget.opens.Load())
	}
}

func TestEngineContentHashesBothSidesConcurrently(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a", "same")
	target.put("backup/a", "same")
	sourceOpen, targetOpen := make(chan struct{}), make(chan struct{})
	meet := func(mine, other chan struct{}) func(context.Context, string, []byte, int) (io.ReadCloser, bool, error) {
		return func(ctx context.Context, _ string, _ []byte, _ int) (io.ReadCloser, bool, error) {
			close(mine)
			// Each side waits for the other: serial hashing never gets here.
			select {
			case <-other:
				return nil, false, nil
			case <-time.After(engineHangGuard):
				return nil, true, errors.New("the other side was not read concurrently")
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}
	}
	source.openHook, target.openHook = meet(sourceOpen, targetOpen), meet(targetOpen, sourceOpen)
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch {
		t.Fatalf("concurrent hashing failed: %v %+v", err, rows)
	}
}

func TestEngineContentFailureStopsTheOtherSide(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a", "same")
	target.put("backup/a", "same")
	var targetStopped atomic.Bool
	source.openHook = func(context.Context, string, []byte, int) (io.ReadCloser, bool, error) {
		return nil, true, engineAPIError(403, "AccessDenied", "G1")
	}
	target.openHook = func(ctx context.Context, _ string, _ []byte, _ int) (io.ReadCloser, bool, error) {
		<-ctx.Done()
		targetStopped.Store(true)
		return nil, true, ctx.Err()
	}
	var rows []Row
	var err error
	label := engineLabeled(func(ctx context.Context) {
		rows, err = engineRun(ctx, engineContentRequest(), source, target, nil, RunOptions{})
	})
	engineNoGoroutines(t, label)
	if err != nil || len(rows) != 1 || !targetStopped.Load() {
		t.Fatalf("comparison failed or the target read kept running: %v %+v", err, rows)
	}
	if row := rows[0]; row.Status != StatusError || row.Error != "Cannot read complete object content" || row.ErrorDetail != "source GET: HTTP 403 AccessDenied (request ID G1)" || row.SourceSHA256 != "" {
		t.Fatalf("error row: %+v", row)
	}
	if source.opens.Load() != 1 {
		t.Fatalf("an access denial was retried: %d opens", source.opens.Load())
	}
}

func TestEngineContentChangesDuringReads(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &contentRangeSize, 64<<10)
	data := enginePayload(200<<10, 4)
	for _, test := range []struct {
		name string
		err  error
	}{{"changed", transferstore.ErrSourceChanged}, {"deleted", fmt.Errorf("%w: gone", transferstore.ErrNotFound)}} {
		t.Run(test.name, func(t *testing.T) {
			source, target := engineRangedPair(data)
			source.rangeHook = func(_ context.Context, _ string, _ []byte, offset, _ int64, _ int) (io.ReadCloser, bool, error) {
				if offset == 64<<10 {
					return nil, true, test.err
				}
				return nil, false, nil
			}
			rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
			if err != nil || len(rows) != 1 || rows[0].Status != StatusChanged || rows[0].Error != "" {
				t.Fatalf("change during read: %v %+v", err, rows)
			}
			if attempts := source.attempt("range src/obj@65536") - 1; attempts != 1 {
				t.Fatalf("a changed object was read %d times", attempts)
			}
		})
	}

	// After both hashes, a changed object is still caught by the second HEAD.
	source, target := engineRangedPair(data)
	var heads atomic.Int64
	target.statHook = func(context.Context, string) error {
		if heads.Add(1) == 2 {
			changed := slices.Clone(data)
			changed[0]++
			target.putObject("backup/obj", engineObject{data: changed, etag: engineMD5ETag(changed)})
		}
		return nil
	}
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusChanged {
		t.Fatalf("change after hashing: %v %+v", err, rows)
	}
}

func TestEngineContentReportsSizeAndHashReasons(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/hash", "AAAA")
	target.put("backup/hash", "BBBB")
	source.put("src/size", "AAAA")
	target.put("backup/size", "AAAAA")
	target.putObject("backup/meta", engineObject{data: []byte("same"), etag: `"opaque-1"`, contentType: "text/plain"})
	source.putObject("src/meta", engineObject{data: []byte("same"), etag: `"opaque-2"`})
	rows, err := engineRun(context.Background(), engineContentRequest(), source, target, nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := engineRowsByKey(t, rows)
	if row := got["hash"]; row.Status != StatusDifferent || !slices.Equal(row.Reasons, []string{ReasonSHA256}) || row.SourceSHA256 != engineSHA256([]byte("AAAA")) || row.DestinationSHA256 != engineSHA256([]byte("BBBB")) {
		t.Fatalf("hash difference: %+v", row)
	}
	if row := got["size"]; row.Status != StatusDifferent || !slices.Equal(row.Reasons, []string{ReasonSize}) || row.SourceSHA256 != "" {
		t.Fatalf("size difference: %+v", row)
	}
	// Content mode compares bytes only: metadata and opaque ETags do not matter.
	if row := got["meta"]; row.Status != StatusMatch || len(row.Reasons) != 0 {
		t.Fatalf("same content: %+v", row)
	}
	if source.opens.Load() != 2 || target.opens.Load() != 2 {
		t.Fatalf("read %d/%d objects, want 2/2 (no read for a size difference)", source.opens.Load(), target.opens.Load())
	}
}
