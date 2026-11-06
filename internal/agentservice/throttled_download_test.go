package agentservice

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A throttled task whose share covers many chunks a second downloads to LOCAL
// in ranges, and its source bytes still never outrun the share: the bucket
// grants no initial burst and at most one credit window, so the copy cannot
// finish before (size - burst) / rate.
func TestThrottledLocalDownloadRangesWithinTaskShare(t *testing.T) {
	const chunk = 256 << 10
	const rate = 8 << 20
	const burst = rate / 10 // the limiter's credit window: 100 ms of the rate
	payload := bytes.Repeat([]byte("0123456789abcdef"), 32*chunk/16)
	bucket := &rangedBucket{data: payload, parts: map[int][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(bucket.serve))
	defer server.Close()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limits, err := ratelimit.New(db, ratelimit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer limits.Close()
	nodes, err := nodecontrol.New(db, limits, nodecontrol.Options{UseZeroDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	defer nodes.Close()
	if err := limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: 9, Generation: 1, Revision: 1, BandwidthBytesPerSecond: rate}); err != nil {
		t.Fatal(err)
	}
	memory, _ := transferstore.NewUploadMemoryBudget(64 << 20)
	lender := transferstore.NewPartLender()
	client := &http.Client{Transport: nodes.Transport(server.Client().Transport, nodecontrol.RequestContext{TaskID: 9, Generation: 1, Bucket: "bucket", PathStyle: true})}
	root := t.TempDir()
	factory := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		source, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, RangedReadMinSize: 1, RetryMaxAttempts: 1, HTTPClient: client})
		if err != nil {
			return nil, nil, err
		}
		target, err := transferstore.NewLocal(root)
		if err != nil {
			return nil, nil, err
		}
		return source, target, target.SetRangedWrites(transferstore.LocalRangedOptions{MinSize: 1, ChunkSize: chunk, PartConcurrency: 4, PartLender: lender, Memory: memory})
	})
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rates := NewTaskRates()
	engine, err := execution.NewEngine(repo, factory, execution.Options{DecorateReader: rates.Decorator(nodes.Decorator(limits.Decorator)), TaskLimits: taskLimits(limits)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	lender.SetIdle(engine.IdleObjectSlots)
	job := execution.Job{ChildID: "9-1", ParentID: 9, Number: 1, Generation: 1, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, RangedDownload: execution.RangedDownloadAuto,
		Source: execution.Endpoint{Kind: "S3", Ref: "src", Bucket: "bucket"}, Destination: execution.Endpoint{Kind: "LOCAL", Ref: "dst"},
		Items: []execution.Item{{ID: "1", Source: transferstore.Object{Key: "source", Size: int64(len(payload)), ETag: "source-etag"}, DestinationKey: "target.bin"}}}
	if _, err := engine.Accept(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := engine.Start(context.Background(), job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	record, err := engine.Wait(ctx, job.ChildID)
	elapsed := time.Since(start)
	if err != nil || record.Status != execution.Completed || record.Counters.SucceededBytes != int64(len(payload)) {
		t.Fatalf("record %+v %v", record, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "target.bin")); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("target differs from source: %v", err)
	}
	bucket.mu.Lock()
	gets := len(bucket.gets)
	bucket.mu.Unlock()
	// One GET per chunk (plus a probe); one stream would be a single GET.
	if gets < 32 {
		t.Fatalf("throttled download did not range: %d GETs", gets)
	}
	if minimum := time.Duration(float64(len(payload)-burst) / rate * float64(time.Second)); elapsed < minimum {
		t.Fatalf("throttled download exceeded its share: %d bytes in %v, minimum %v", len(payload), elapsed, minimum)
	}
	if lender.Lent() != 0 {
		t.Fatal("borrowed slots outlived the copy", lender.Lent())
	}
}
