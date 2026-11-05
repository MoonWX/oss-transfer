package execution

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

const localTestPart = 256 << 10

// countingTransport stands in for the node request limiter: every request the
// S3 client makes passes through it once.
type countingTransport struct {
	base         http.RoundTripper
	gets, ranged atomic.Int64
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet {
		c.gets.Add(1)
		if request.Header.Get("Range") != "" {
			c.ranged.Add(1)
		}
	}
	return c.base.RoundTrip(request)
}

// downloadFixture serves payload from a fault-free S3 fake and opens a Local
// target that writes objects of more than one 256 KiB range in ranges.
func downloadFixture(t *testing.T, payload []byte, concurrency int, lender *transferstore.PartLender) (*transferstore.S3, *countingTransport, *transferstore.Local, string, transferstore.Object) {
	t.Helper()
	server := newFaultS3(payload, localTestPart, faultConfig{MutateAt: -1}, 1)
	t.Cleanup(server.Close)
	counter := &countingTransport{base: server.server.Client().Transport}
	source, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, RetryMaxAttempts: 1, HTTPClient: &http.Client{Transport: counter}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	target, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if err := target.SetRangedWrites(transferstore.LocalRangedOptions{MinSize: 1, PartSize: localTestPart, PartConcurrency: concurrency, PartLender: lender}); err != nil {
		t.Fatal(err)
	}
	stat, err := source.Stat(bg, "source")
	if err != nil {
		t.Fatal(err)
	}
	counter.gets.Store(0)
	counter.ranged.Store(0)
	return source, counter, target, root, stat
}

func downloadPayload(size int) []byte {
	rng := rand.New(rand.NewPCG(uint64(size), 2))
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(rng.Uint32())
	}
	return payload
}

func s3ToLocalJob(parent int64) Job {
	return Job{ParentID: parent, Generation: 1, Source: Endpoint{Kind: "S3"}, Destination: Endpoint{Kind: "LOCAL"}, ConflictPolicy: "OVERWRITE"}
}

// monotonicSampler records the reported progress after every read, in one
// global order, so a decrease anywhere is visible.
type monotonicSampler struct {
	io.ReadCloser
	object  *objectProgress
	mu      *sync.Mutex
	samples *[]int64
}

func (r *monotonicSampler) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	*r.samples = append(*r.samples, r.object.transferred())
	r.mu.Unlock()
	return n, err
}

func TestS3ToLocalDownloadsRangesWithLimiterProgressAndRequests(t *testing.T) {
	const parts = 11
	payload := downloadPayload(parts*localTestPart - 777)
	source, counter, target, root, stat := downloadFixture(t, payload, 4, nil)
	object := &objectProgress{item: ObjectProgress{Size: stat.Size}}
	var mu sync.Mutex
	var samples []int64
	sizes := map[int64]int{}
	var calls atomic.Int64
	engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
		mu.Lock()
		sizes[part.Source.Size]++
		mu.Unlock()
		var reader io.ReadCloser = input
		if calls.Add(1) == 3 { // interrupted halfway, then repeated
			reader = &failingRange{ReadCloser: input, remaining: part.Source.Size / 2}
		}
		return &monotonicSampler{ReadCloser: reader, object: object, mu: &mu, samples: &samples}, nil
	}}}
	item := Item{ID: "1", Source: stat, DestinationKey: "dir/large.bin"}
	result := engine.copyItem(bg, s3ToLocalJob(1), item, source, target, object)
	if result.Status != ItemSucceeded || !result.TargetCommitted {
		t.Fatalf("download failed: %+v", result)
	}
	if got, err := os.ReadFile(filepath.Join(root, "dir", "large.bin")); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("target differs from source: %v", err)
	}
	// Each range, and the repeated one, went through the limiter decorator
	// with its own length, and each was one GET counted by the request layer.
	last := stat.Size - (parts-1)*localTestPart
	if calls.Load() != parts+1 || sizes[localTestPart]+sizes[last] != parts+1 || sizes[last] < 1 {
		t.Fatalf("decorations %d %v", calls.Load(), sizes)
	}
	if counter.ranged.Load() != parts+2 || counter.gets.Load() != parts+2 {
		t.Fatalf("requests: %d GETs, %d ranged; want probe plus %d ranges", counter.gets.Load(), counter.ranged.Load(), parts+1)
	}
	if got := object.bytes.Load(); got != stat.Size {
		t.Fatalf("live progress %d, want %d", got, stat.Size)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i] < samples[i-1] || samples[i] > stat.Size {
			t.Fatalf("progress moved from %d to %d (size %d)", samples[i-1], samples[i], stat.Size)
		}
	}
	if len(samples) == 0 || object.transferred() != stat.Size {
		t.Fatal("progress did not reach the object size")
	}
}

func TestThrottledLocalDownloadStreamsOneRequest(t *testing.T) {
	payload := downloadPayload(8*localTestPart + 5)
	lender := idlePartLender(100)
	source, counter, target, root, stat := downloadFixture(t, payload, 4, lender)
	var throttled atomic.Bool
	var mu sync.Mutex
	var sizes []int64
	engine := &Engine{opts: Options{
		TaskThrottled: func(Job) bool { return throttled.Load() },
		DecorateReader: func(_ context.Context, _ Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
			mu.Lock()
			sizes = append(sizes, part.Source.Size)
			mu.Unlock()
			return input, nil
		},
	}}
	copyOnce := func(key string) (int64, int64) {
		t.Helper()
		mu.Lock()
		sizes = nil
		mu.Unlock()
		counter.gets.Store(0)
		counter.ranged.Store(0)
		if result := engine.copyItem(bg, s3ToLocalJob(1), Item{ID: key, Source: stat, DestinationKey: key}, source, target); result.Status != ItemSucceeded {
			t.Fatalf("copy failed: %+v", result)
		}
		if got, _ := os.ReadFile(filepath.Join(root, key)); !bytes.Equal(got, payload) {
			t.Fatal("target differs from source")
		}
		return counter.gets.Load(), counter.ranged.Load()
	}
	throttled.Store(true)
	if gets, ranged := copyOnce("throttled.bin"); gets != 1 || ranged != 0 || len(sizes) != 1 || sizes[0] != stat.Size {
		t.Fatalf("throttled download: gets=%d ranged=%d decorations=%v", gets, ranged, sizes)
	}
	if lender.Lent() != 0 {
		t.Fatal("throttled download borrowed slots")
	}
	throttled.Store(false)
	if gets, ranged := copyOnce("open.bin"); gets != 10 || ranged != 10 || len(sizes) != 9 {
		t.Fatalf("unthrottled download: gets=%d ranged=%d decorations=%v", gets, ranged, sizes)
	}
}

// A lone large download uses idle node capacity beyond its base slot, and
// returns every borrowed slot when it ends.
func TestLoneLocalDownloadBorrowsIdleSlots(t *testing.T) {
	const parts, overlap = 8, 5
	payload := downloadPayload(parts * localTestPart)
	lender := idlePartLender(100)
	source, _, target, root, stat := downloadFixture(t, payload, 1, lender)
	var mu sync.Mutex
	arrived, active, peak := 0, 0, 0
	all := make(chan struct{})
	engine := &Engine{opts: Options{DecorateReader: func(ctx context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		mu.Lock()
		arrived++
		active++
		peak = max(peak, active)
		if arrived == overlap {
			close(all)
		}
		held := arrived <= overlap
		mu.Unlock()
		if held {
			select {
			case <-all:
			case <-ctx.Done():
			case <-time.After(time.Minute): // hang guard only
			}
		}
		return &closeHook{ReadCloser: input, closed: func() { mu.Lock(); active--; mu.Unlock() }}, nil
	}}}
	if result := engine.copyItem(bg, s3ToLocalJob(1), Item{ID: "1", Source: stat, DestinationKey: "large.bin"}, source, target); result.Status != ItemSucceeded {
		t.Fatalf("copy failed: %+v", result)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "large.bin")); !bytes.Equal(got, payload) {
		t.Fatal("target differs from source")
	}
	if peak < overlap || lender.Lent() != 0 {
		t.Fatalf("peak %d concurrent ranges, %d slots still lent", peak, lender.Lent())
	}
}

type closeHook struct {
	io.ReadCloser
	once   sync.Once
	closed func()
}

func (r *closeHook) Close() error {
	r.once.Do(r.closed)
	return r.ReadCloser.Close()
}

// A withdrawn (retried) range lowers the live byte count, but reported
// progress keeps its high-water mark and never exceeds the object size.
func TestObjectProgressReportsMonotonicHighWaterMark(t *testing.T) {
	object := &objectProgress{item: ObjectProgress{Size: 1000}}
	steps := []struct{ add, want int64 }{{100, 100}, {300, 400}, {-300, 400}, {200, 400}, {250, 550}, {-50, 550}, {600, 1000}, {-1000, 1000}}
	for _, step := range steps {
		object.bytes.Add(step.add)
		if got := object.transferred(); got != step.want {
			t.Fatalf("after %+d: reported %d, want %d", step.add, got, step.want)
		}
	}
	p := newAttemptProgress(bg, Record{Job: Job{Items: []Item{{ID: "a", Source: transferstore.Object{Size: 1000}}}}})
	p.initialize(0)
	live := p.begin(Item{ID: "a", Source: transferstore.Object{Size: 1000}})
	live.bytes.Store(700)
	if child, items, _ := p.snapshot(true); child.InFlightBytes != 700 || items[0].TransferredBytes != 700 {
		t.Fatal(child, items)
	}
	live.bytes.Store(200)
	if child, _, _ := p.snapshot(false); child.InFlightBytes != 700 {
		t.Fatal("summary progress moved backwards", child)
	}
}

// Engine.copyItem from a fault-injecting S3 source into a ranged Local
// target. A success must publish exactly the source bytes with exact live
// progress; a failure must publish nothing and leave no temporary file.
func TestEngineRangedDownloadIntegrityFuzz(t *testing.T) {
	scenarios := fuzzEnv("RANGED_FUZZ_ENGINE_LOCAL", 30)
	base := uint64(fuzzEnv("RANGED_FUZZ_SEED", 20261103))
	retryable := []fsGet{gTruncate, gResetBody, gSlow, g500}
	detectable := []fsGet{gFull200, gFull200CR, gWrongCR, gNoCR, gWrongTotal, gChunkShort, gChunkLong, gBadLength, gNoETag, g412, gTruncate, gResetBody, gSlow}
	outcomes := map[string]int{}
	var mu sync.Mutex
	var group sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range scenarios {
		seed := base + uint64(i)
		root := t.TempDir()
		group.Add(1)
		sem <- struct{}{}
		go func() {
			defer group.Done()
			defer func() { <-sem }()
			rng := rand.New(rand.NewPCG(seed, 4))
			size := int64(localTestPart + 1 + rng.Int64N(12*localTestPart))
			data := make([]byte, size)
			for j := range data {
				data[j] = byte(rng.Uint32())
			}
			cfg := faultConfig{MutateAt: -1}
			mode := rng.IntN(5)
			switch mode {
			case 1:
				cfg.GetKinds, cfg.GetRate = retryable, 0.3
			case 2:
				cfg.GetKinds, cfg.GetRate, cfg.ProbeFaults = detectable, 0.2, rng.IntN(2) == 0
			case 3:
				cfg.MutateAt, cfg.IgnoreIfMatch = rng.IntN(6), rng.IntN(2) == 0
				cfg.GetKinds, cfg.GetRate = []fsGet{gSlow}, 0.5
			case 4:
				cfg.GetKinds, cfg.GetRate, cfg.ProbeFaults = []fsGet{gFull200, gWrongCR, gNoCR}, 1, true // probe unsupported: streaming fallback
			}
			server := newFaultS3(data, localTestPart, cfg, seed)
			defer server.Close()
			source, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, RetryMaxAttempts: 3, HTTPClient: server.server.Client()})
			if err != nil {
				t.Error(err)
				return
			}
			target, err := transferstore.NewLocal(root)
			if err != nil {
				t.Error(err)
				return
			}
			defer target.Close()
			lender := idlePartLender(rng.IntN(2) * 50)
			if err := target.SetRangedWrites(transferstore.LocalRangedOptions{MinSize: 1, PartSize: localTestPart, PartConcurrency: 1 + rng.IntN(6), PartLender: lender}); err != nil {
				t.Error(err)
				return
			}
			var decoratorRng sync.Mutex
			engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
				decoratorRng.Lock()
				defer decoratorRng.Unlock()
				return &chunkyReader{ReadCloser: input, rng: rand.New(rand.NewPCG(seed, uint64(part.Source.Size)+rng.Uint64()))}, nil
			}}}
			item := Item{ID: "1", Source: transferstore.Object{Key: "source", Size: size, ETag: fsETag(data)}, DestinationKey: "target"}
			object := &objectProgress{item: ObjectProgress{Size: size}}
			ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
			defer cancel()
			if mode != 0 && mode != 4 && rng.IntN(6) == 0 {
				timer := time.AfterFunc(time.Duration(rng.Int64N(int64(300*time.Millisecond))), cancel)
				defer timer.Stop()
			}
			result := engine.copyItem(ctx, s3ToLocalJob(1), item, source, target, object)
			got, readErr := os.ReadFile(filepath.Join(root, "target"))
			entries, _ := os.ReadDir(root)
			mu.Lock()
			defer mu.Unlock()
			outcomes[fmt.Sprintf("mode%d:%s", mode, result.Status)]++
			desc := fmt.Sprintf("seed=%d mode=%d size=%d", seed, mode, size)
			if result.Status == ItemSucceeded {
				if readErr != nil || !bytes.Equal(got, data) {
					t.Errorf("CRITICAL %s: item succeeded with different content (%v)", desc, readErr)
				}
				if counted := object.bytes.Load(); counted != size {
					t.Errorf("%s: live progress %d of %d", desc, counted, size)
				}
			} else if readErr == nil || result.TargetCommitted {
				t.Errorf("CRITICAL %s: item failed (%s) but a target was published", desc, result.Error)
			}
			if len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "target") {
				t.Errorf("CRITICAL %s: temporary file left behind: %v", desc, entries)
			}
			if lender.Lent() != 0 {
				t.Errorf("%s: %d borrowed slots not returned", desc, lender.Lent())
			}
			if (mode == 0 || mode == 4) && result.Status != ItemSucceeded {
				t.Errorf("fault-free scenario failed %s: %s", desc, result.Error)
			}
		}()
	}
	group.Wait()
	t.Logf("engine download outcomes %v", outcomes)
}
