package execution

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// chunkyReader returns random short reads and sometimes merges EOF into the
// final data read, like real limiters and TLS readers do.
type chunkyReader struct {
	io.ReadCloser
	mu  sync.Mutex
	rng *rand.Rand
}

func (r *chunkyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	limit := 1 + r.rng.IntN(64<<10)
	merge := r.rng.IntN(2) == 0
	r.mu.Unlock()
	if len(p) > limit {
		p = p[:limit]
	}
	n, err := r.ReadCloser.Read(p)
	if err == nil && merge && n > 0 {
		var one [1]byte
		if m, e := r.ReadCloser.Read(one[:0]); m == 0 && e == io.EOF {
			return n, io.EOF
		}
	}
	return n, err
}

func fuzzEnv(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return value
	}
	return fallback
}

// Engine.copyItem from a fault-injecting S3 source to the same server's
// multipart target, through openSource, rangeReader, progressRange and a
// decorator. Every success must commit exactly the original source bytes.
func TestEngineRangedCopyIntegrityFuzz(t *testing.T) {
	scenarios := fuzzEnv("RANGED_FUZZ_ENGINE", 30)
	base := uint64(fuzzEnv("RANGED_FUZZ_SEED", 20261103))
	retryable := []fsGet{gTruncate, gResetBody, gSlow, g500}
	detectable := []fsGet{gFull200, gFull200CR, gWrongCR, gNoCR, gWrongTotal, gChunkShort, gChunkLong, gBadLength, gNoETag, g412, gTruncate, gResetBody, gSlow}
	outcomes := map[string]int{}
	var mu sync.Mutex
	var group sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range scenarios {
		seed := base + uint64(i)
		group.Add(1)
		sem <- struct{}{}
		go func() {
			defer group.Done()
			defer func() { <-sem }()
			rng := rand.New(rand.NewPCG(seed, 3))
			const partSize = 5 << 20
			size := int64(partSize + 1 + rng.Int64N(4*partSize))
			data := make([]byte, size)
			for j := range data {
				data[j] = byte(rng.Uint32())
			}
			cfg := faultConfig{MutateAt: -1}
			mode := rng.IntN(5)
			switch mode {
			case 1:
				cfg.GetKinds, cfg.GetRate = retryable, 0.3
				cfg.PutKinds, cfg.PutRate = []fsPut{pSlow, pStored500, pLostResponse}, 0.3
			case 2:
				cfg.GetKinds, cfg.GetRate, cfg.ProbeFaults = detectable, 0.2, rng.IntN(2) == 0
			case 3:
				cfg.MutateAt, cfg.IgnoreIfMatch = rng.IntN(6), rng.IntN(2) == 0
				cfg.GetKinds, cfg.GetRate = []fsGet{gSlow}, 0.5
			case 4:
				cfg.GetKinds, cfg.GetRate, cfg.ProbeFaults = []fsGet{gFull200, gWrongCR, gNoCR}, 1, true // probe unsupported: streaming fallback
			}
			server := newFaultS3(data, partSize, cfg, seed)
			defer server.Close()
			budget, _ := transferstore.NewUploadMemoryBudget([]int64{partSize, 3 * partSize, 64 << 20}[rng.IntN(3)])
			lender := transferstore.NewPartLender()
			idle := rng.IntN(2) * 50
			lender.SetIdle(func() int { return idle })
			config := transferstore.S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: partSize, PartConcurrency: 1 + rng.IntN(6), UploadMemory: budget, PartLender: lender, RangedReadMinSize: 1, RetryMaxAttempts: 3, HTTPClient: server.server.Client()}
			store, err := transferstore.NewS3(config)
			if err != nil {
				t.Error(err)
				return
			}
			var decoratorRng sync.Mutex
			engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, part Item, input io.ReadCloser) (io.ReadCloser, error) {
				decoratorRng.Lock()
				defer decoratorRng.Unlock()
				return &chunkyReader{ReadCloser: input, rng: rand.New(rand.NewPCG(seed, uint64(part.Source.Size)+uint64(time.Now().UnixNano())))}, nil
			}}}
			item := Item{ID: "1", Source: transferstore.Object{Key: "source", Size: size, ETag: fsETag(data)}, DestinationKey: "target"}
			object := &objectProgress{item: ObjectProgress{Size: size}}
			job := Job{Source: Endpoint{Kind: "S3"}, Destination: Endpoint{Kind: "S3"}, ConflictPolicy: "OVERWRITE"}
			ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
			defer cancel()
			if mode != 0 && mode != 4 && rng.IntN(6) == 0 {
				timer := time.AfterFunc(time.Duration(rng.Int64N(int64(300*time.Millisecond))), cancel)
				defer timer.Stop()
			}
			result := engine.copyItem(ctx, job, item, store, store, object)
			completed, _, targetOK, _ := server.State()
			desc := fmt.Sprintf("seed=%d mode=%d size=%d", seed, mode, size)
			mu.Lock()
			defer mu.Unlock()
			outcomes[fmt.Sprintf("mode%d:%s", mode, result.Status)]++
			for _, v := range server.Violations() {
				t.Errorf("CRITICAL %s: %s", desc, v)
			}
			if result.Status == ItemSucceeded {
				if completed != 1 || !targetOK {
					t.Errorf("CRITICAL %s: item succeeded but completed=%d targetOK=%v", desc, completed, targetOK)
				}
				if got := object.bytes.Load(); got != size {
					t.Logf("progress %s: counted %d of %d", desc, got, size)
				}
			} else if completed != 0 && ctx.Err() == nil {
				t.Errorf("CRITICAL %s: item failed (%s) but upload completed", desc, result.Error)
			}
			if mode == 0 && result.Status != ItemSucceeded {
				t.Errorf("clean scenario failed %s: %s", desc, result.Error)
			}
			if mode == 4 && result.Status != ItemSucceeded {
				t.Logf("fallback scenario %s: %s", desc, result.Error)
			}
		}()
	}
	group.Wait()
	t.Logf("engine outcomes %v", outcomes)
}
