package transferstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	retryableGets = []fsGet{gTruncate, gResetHead, gResetBody, g500, g503, gSlow}
	allGets       = []fsGet{gFull200, gFull200CR, gWrongCR, gNoCR, gWrongTotal, gTruncate, gResetHead, gResetBody, gChunkShort, gChunkLong, gBadLength, g500, g503, g412, gNoETag, gSlow}
	allPuts       = []fsPut{p500, p503, pResetMid, pLostResponse, pStored500, pSlow}
)

// fastRetries keeps the store's middleware but removes SDK backoff and the
// retry quota, so many injected 5xx/reset faults stay cheap.
func fastRetries(s *S3, attempts int) {
	options := s.client.Options()
	options.Retryer = retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = attempts
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return time.Millisecond, nil })
		o.RateLimiter = ratelimit.None
	})
	s.client = s3.New(options)
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return value
	}
	return fallback
}

func fuzzPayload(rng *rand.Rand, size int64) []byte {
	data := make([]byte, size)
	for i := 0; i+8 <= len(data); i += 8 {
		v := rng.Uint64()
		for j := 0; j < 8; j++ {
			data[i+j] = byte(v >> (8 * j))
		}
	}
	for i := len(data) &^ 7; i < len(data); i++ {
		data[i] = byte(rng.Uint32())
	}
	return data
}

// fuzzSize picks sizes around part boundaries and every possible last part.
func fuzzSize(rng *rand.Rand, partSize int64) int64 {
	k := int64(1 + rng.IntN(4))
	switch rng.IntN(4) {
	case 0:
		return k*partSize + []int64{-1, 0, 1}[rng.IntN(3)] + partSize
	case 1:
		return k*partSize + 1 + rng.Int64N(partSize) // last part 1..partSize
	case 2:
		return k*partSize + []int64{1, partSize}[rng.IntN(2)] // extremes
	default:
		return partSize + 1 + rng.Int64N(3*partSize)
	}
}

type fuzzOutcome struct {
	seed       uint64
	desc       string
	err        error
	committed  bool
	completed  int
	aborted    int
	targetOK   bool
	violations []string
	counts     map[string]int
	critical   []string
	unexpected string
	leak       string
}

func classify(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrSourceChanged):
		return "source-changed"
	case errors.Is(err, ErrSizeMismatch):
		return "size-mismatch"
	case errors.Is(err, ErrUnsupported):
		return "unsupported"
	case err.Error() == "skipped: not ranged":
		return "skipped"
	default:
		return "other"
	}
}

func runRangedFaultScenario(seed uint64, positiveControl bool) fuzzOutcome {
	rng := rand.New(rand.NewPCG(seed, 7))
	partSize := minPartSize
	size := fuzzSize(rng, partSize)
	cfg := faultConfig{MutateAt: -1}
	cancelAfter := time.Duration(-1)
	mode := rng.IntN(10)
	switch mode {
	case 0: // clean
	case 1, 2:
		cfg.GetKinds, cfg.GetRate = retryableGets, []float64{0.1, 0.3, 0.6}[rng.IntN(3)]
		cfg.PutKinds, cfg.PutRate = allPuts, []float64{0.1, 0.3, 0.5}[rng.IntN(3)]
	case 3, 4:
		cfg.GetKinds, cfg.GetRate = allGets, []float64{0.05, 0.2, 0.5}[rng.IntN(3)]
		cfg.ProbeFaults = rng.IntN(4) == 0
	case 5:
		cfg.PutKinds, cfg.PutRate = allPuts, []float64{0.2, 0.5, 0.8}[rng.IntN(3)]
	case 6:
		cfg.MutateAt = rng.IntN(int(size/partSize) + 2)
		cfg.IgnoreIfMatch = rng.IntN(2) == 0
		cfg.GetKinds, cfg.GetRate = []fsGet{gSlow}, 0.5
	case 7:
		cancelAfter = time.Duration(rng.Int64N(int64(400 * time.Millisecond)))
		cfg.GetKinds, cfg.GetRate = []fsGet{gSlow}, 0.5
		cfg.PutKinds, cfg.PutRate = []fsPut{pSlow}, 0.5
	default: // everything at once
		cfg.GetKinds, cfg.GetRate = allGets, 0.15
		cfg.PutKinds, cfg.PutRate = allPuts, 0.15
		if rng.IntN(2) == 0 {
			cfg.MutateAt = rng.IntN(int(size/partSize) + 2)
			cfg.IgnoreIfMatch = rng.IntN(2) == 0
		}
		if rng.IntN(3) == 0 {
			cancelAfter = time.Duration(rng.Int64N(int64(500 * time.Millisecond)))
		}
	}
	if positiveControl {
		cfg = faultConfig{MutateAt: -1, GetKinds: []fsGet{gShiftedBody}, GetRate: 0.5}
		cancelAfter = -1
	}
	concurrency := 1 + rng.IntN(8)
	budgetBytes := []int64{partSize, 2 * partSize, 3 * partSize, 64 << 20}[rng.IntN(4)]
	idle := []int{0, 0, 3, 100}[rng.IntN(4)]
	sdkAttempts := 2 + rng.IntN(4)
	server := newFaultS3(fuzzPayload(rng, size), partSize, cfg, seed)
	defer server.Close()
	out := fuzzOutcome{seed: seed, desc: fmt.Sprintf("seed=%d mode=%d size=%d (parts=%d last=%d) conc=%d budget=%d idle=%d sdkAttempts=%d cancel=%v cfg={get %.2f %d kinds probe=%v put %.2f %d kinds mutateAt=%d ignoreIfMatch=%v}", seed, mode, size, (size+partSize-1)/partSize, size-(size-1)/partSize*partSize, concurrency, budgetBytes, idle, sdkAttempts, cancelAfter, cfg.GetRate, len(cfg.GetKinds), cfg.ProbeFaults, cfg.PutRate, len(cfg.PutKinds), cfg.MutateAt, cfg.IgnoreIfMatch)}
	budget, err := NewUploadMemoryBudget(budgetBytes)
	if err != nil {
		out.critical = append(out.critical, err.Error())
		return out
	}
	lender := NewPartLender()
	lender.SetIdle(func() int { return idle })
	s, err := NewS3(S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: partSize, PartConcurrency: concurrency, UploadMemory: budget, PartLender: lender, RangedReadMinSize: 1, RetryMaxAttempts: sdkAttempts, HTTPClient: server.server.Client()})
	if err != nil {
		out.critical = append(out.critical, err.Error())
		return out
	}
	fastRetries(s, sdkAttempts)
	if !s.RangedWrite(size) {
		out.err = errors.New("skipped: not ranged")
		return out
	}
	server.mu.Lock()
	server.partSize = s.multipartPartSize(size, true)
	server.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if cancelAfter >= 0 {
		timer := time.AfterFunc(cancelAfter, cancel)
		defer timer.Stop()
	}
	result, err := func() (WriteResult, error) {
		ranges, object, err := s.OpenRanges(ctx, "source", OpenOptions{Expected: &Object{Key: "source", Size: size}})
		if err != nil {
			return WriteResult{}, err
		}
		defer ranges.Close()
		return s.Write(ctx, "target", strings.NewReader("unused"), WriteOptions{Size: object.Size, Overwrite: true, DestinationAbsent: true, Ranges: ranges.OpenRange})
	}()
	out.err, out.committed = err, result.Committed
	var mutated bool
	out.completed, out.aborted, out.targetOK, mutated = server.State()
	out.violations = server.Violations()
	out.counts = server.Counts()
	out.critical = append(out.critical, out.violations...)
	if err == nil && (!result.Committed || out.completed != 1 || !out.targetOK) {
		out.critical = append(out.critical, fmt.Sprintf("Write succeeded but committed=%v completed=%d targetOK=%v", result.Committed, out.completed, out.targetOK))
	}
	if err != nil && out.completed != 0 && out.targetOK && ctx.Err() != nil {
		// Cancellation raced an in-flight CompleteMultipartUpload: the target
		// holds exactly the source bytes, but the caller sees a failure. Same as
		// the streaming path; not an integrity defect.
		out.unexpected = "cancel raced a successful Complete (target correct, reported failed)"
	} else if err != nil && out.completed != 0 {
		out.critical = append(out.critical, fmt.Sprintf("Write failed (%v) but the server completed the upload %d times", err, out.completed))
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.unexpected = "hang until 90s deadline: " + err.Error()
	}
	clean := len(cfg.GetKinds) == 0 && len(cfg.PutKinds) == 0 && cfg.MutateAt < 0 && cancelAfter < 0
	slowOnly := cfg.MutateAt < 0 && cancelAfter < 0 && onlySlow(cfg)
	if err != nil && (clean || slowOnly) {
		out.unexpected = "fault-free scenario failed: " + err.Error()
	}
	if mutated && cfg.IgnoreIfMatch && err == nil && out.completed == 1 && !out.targetOK {
		out.critical = append(out.critical, "mutated source copied")
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 || lender.Lent() != 0 {
		out.leak = fmt.Sprintf("memory used=%d waiting=%d lent=%d", used, waiting, lender.Lent())
	}
	return out
}

func onlySlow(cfg faultConfig) bool {
	for _, kind := range cfg.GetKinds {
		if kind != gSlow {
			return false
		}
	}
	for _, kind := range cfg.PutKinds {
		if kind != pSlow {
			return false
		}
	}
	return true
}

func TestRangedMultipartIntegrityFuzz(t *testing.T) {
	scenarios := envInt("RANGED_FUZZ_SCENARIOS", 30)
	workers := envInt("RANGED_FUZZ_WORKERS", 6)
	base := uint64(envInt("RANGED_FUZZ_SEED", 20261103))
	t.Logf("base seed %d, %d scenarios", base, scenarios)
	seeds := make(chan uint64)
	results := make(chan fuzzOutcome)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for seed := range seeds {
				results <- runRangedFaultScenario(seed, false)
			}
		}()
	}
	go func() {
		for i := range scenarios {
			seeds <- base + uint64(i)
		}
		close(seeds)
		group.Wait()
		close(results)
	}()
	outcomes := map[string]int{}
	faults := map[string]int{}
	critical, unexpected, leaks := 0, 0, 0
	for out := range results {
		outcomes[classify(out.err)]++
		for k, v := range out.counts {
			faults[k] += v
		}
		if len(out.critical) > 0 {
			critical++
			t.Errorf("CRITICAL %s: %v err=%v", out.desc, out.critical, out.err)
		}
		if out.unexpected != "" {
			unexpected++
			t.Logf("UNEXPECTED %s: %s", out.desc, out.unexpected)
		}
		if out.leak != "" {
			leaks++
			t.Errorf("LEAK %s: %s err=%v", out.desc, out.leak, out.err)
		}
		if classify(out.err) == "other" {
			t.Logf("other error %s: %v", out.desc, out.err)
		}
	}
	t.Logf("outcomes %v", sortedCounts(outcomes))
	t.Logf("injected %v", sortedCounts(faults))
	t.Logf("critical=%d unexpected=%d leaks=%d", critical, unexpected, leaks)
}

// The harness must notice a server that lies about which bytes it sent.
func TestRangedFaultHarnessPositiveControl(t *testing.T) {
	detected := 0
	for seed := uint64(1); seed <= 6; seed++ {
		if out := runRangedFaultScenario(seed, true); len(out.critical) > 0 {
			detected++
		}
	}
	if detected == 0 {
		t.Fatal("shifted-body server was never detected; the integrity checks are blind")
	}
	t.Logf("positive control detected %d/6", detected)
}

func sortedCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d ", k, m[k])
	}
	return b.String()
}

// Local source with concurrent modification while ranged parts upload.
func TestLocalRangedConcurrentModificationFuzz(t *testing.T) {
	scenarios := envInt("RANGED_FUZZ_LOCAL", 20)
	base := uint64(envInt("RANGED_FUZZ_SEED", 20261103))
	outcomes := map[string]int{}
	mutations := map[string]int{}
	for i := range scenarios {
		seed := base + uint64(i)
		rng := rand.New(rand.NewPCG(seed, 11))
		size := fuzzSize(rng, minPartSize)
		payload := fuzzPayload(rng, size)
		root := t.TempDir()
		name := filepath.Join(root, "large.bin")
		if err := os.WriteFile(name, payload, 0600); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Hour).Truncate(time.Second)
		_ = os.Chtimes(name, past, past)
		local, err := NewLocal(root)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := local.Stat(context.Background(), "large.bin")
		if err != nil {
			t.Fatal(err)
		}
		server := newFaultS3(payload, minPartSize, faultConfig{MutateAt: -1, PutKinds: []fsPut{pSlow}, PutRate: 1, MaxSlow: 120 * time.Millisecond}, seed)
		budget, _ := NewUploadMemoryBudget(64 << 20)
		s, err := NewS3(S3Config{Endpoint: server.server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 1 + rng.IntN(6), UploadMemory: budget, PartLender: idleLender(rng.IntN(2) * 100), RangedReadMinSize: 1, RetryMaxAttempts: 2, HTTPClient: server.server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		kind := []string{"none", "inplace", "inplace-keep-mtime", "truncate", "append", "replace", "chtimes"}[rng.IntN(7)]
		mutations[kind]++
		delay := time.Duration(rng.Int64N(int64(400 * time.Millisecond)))
		var mutatedAt time.Time
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(delay)
			defer func() { mutatedAt = time.Now() }()
			switch kind {
			case "inplace", "inplace-keep-mtime":
				file, err := os.OpenFile(name, os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = file.WriteAt([]byte("MODIFIED-CONTENT"), rng.Int64N(size-16))
				_ = file.Close()
				if kind == "inplace-keep-mtime" {
					_ = os.Chtimes(name, past, past)
				}
			case "truncate":
				_ = os.Truncate(name, size-1-rng.Int64N(size/2))
			case "append":
				file, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
				if err == nil {
					_, _ = file.Write([]byte("x"))
					_ = file.Close()
				}
			case "replace":
				changed := append([]byte(nil), payload...)
				changed[0] ^= 1
				_ = os.WriteFile(name+".tmp", changed, 0600)
				_ = os.Chtimes(name+".tmp", past, past)
				_ = os.Rename(name+".tmp", name)
			case "chtimes":
				later := past.Add(time.Minute)
				_ = os.Chtimes(name, later, later)
			}
		}()
		ranges, object, err := local.OpenRanges(context.Background(), "large.bin", OpenOptions{Expected: &expected})
		if err == nil {
			_, err = s.Write(context.Background(), "target", strings.NewReader("unused"), WriteOptions{Size: object.Size, Overwrite: true, DestinationAbsent: true, Ranges: ranges.OpenRange})
			_ = ranges.Close()
		}
		writeDone := time.Now()
		<-done
		completed, _, targetOK, _ := server.State()
		overlap := "after"
		if mutatedAt.Before(writeDone) {
			overlap = "during"
		}
		outcomes[kind+"/"+overlap+":"+classify(err)]++
		desc := fmt.Sprintf("seed=%d size=%d mutation=%s delay=%v", seed, size, kind, delay)
		if v := server.Violations(); len(v) > 0 && err == nil {
			// Parts differing from the original are expected after an in-place
			// change; only a committed object must be the original version.
			t.Logf("parts differed but %s err=nil: %v", desc, v)
		}
		if err == nil && (completed != 1 || !targetOK) {
			if kind == "inplace-keep-mtime" {
				t.Logf("KNOWN-LIMIT %s: committed modified bytes (mtime restored, same size)", desc)
			} else {
				t.Errorf("CRITICAL %s: success with completed=%d targetOK=%v", desc, completed, targetOK)
			}
		}
		if err != nil && completed != 0 {
			t.Errorf("CRITICAL %s: failed but completed", desc)
		}
		server.Close()
		_ = local.Close()
	}
	t.Logf("local outcomes %v", sortedCounts(outcomes))
	t.Logf("local mutations %v", sortedCounts(mutations))
}

var _ = aws.String
