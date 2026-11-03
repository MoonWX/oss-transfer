package agentservice

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// rangedBucket serves "source" with conditional ranges and accepts a multipart
// "target". It records arrival times of object GETs after transport admission.
type rangedBucket struct {
	mu       sync.Mutex
	data     []byte
	parts    map[int][]byte
	target   []byte
	gets     []time.Time
	active   int
	maxParts int
}

func (b *rangedBucket) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch {
	case r.Method == "GET" && key == "source":
		b.mu.Lock()
		b.gets = append(b.gets, time.Now())
		b.mu.Unlock()
		first, last, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
		start, _ := strconv.Atoi(first)
		end, _ := strconv.Atoi(last)
		if r.Header.Get("If-Match") != "\"source-etag\"" || end < start {
			w.WriteHeader(412)
			return
		}
		w.Header().Set("ETag", "\"source-etag\"")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(b.data)))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(206)
		_, _ = w.Write(b.data[start : end+1])
	case r.Method == "POST" && q.Has("uploads"):
		fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>")
	case r.Method == "PUT" && q.Has("partNumber"):
		b.mu.Lock()
		b.active++
		b.maxParts = max(b.maxParts, b.active)
		b.mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		time.Sleep(30 * time.Millisecond)
		number, _ := strconv.Atoi(q.Get("partNumber"))
		b.mu.Lock()
		b.active--
		b.parts[number] = data
		b.mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", number))
	case r.Method == "POST" && q.Has("uploadId"):
		var body struct {
			Parts []struct{ PartNumber int } `xml:"Part"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.target = nil
		for _, part := range body.Parts {
			b.target = append(b.target, b.parts[part.PartNumber]...)
		}
		b.mu.Unlock()
		fmt.Fprint(w, "<CompleteMultipartUploadResult><ETag>\"multipart\"</ETag></CompleteMultipartUploadResult>")
	default:
		w.WriteHeader(400)
	}
}

// The Agent's decorator chain and request transport apply to every source
// range, so task bandwidth and task LIST QPS still cap a ranged copy.
func TestRangedCopyHonoursTaskBandwidthAndRequestLimits(t *testing.T) {
	const partSize = 5 << 20
	for _, tc := range []struct {
		name    string
		limit   ratelimit.ParentLimit
		minimum time.Duration
	}{
		// 40 MiB at 20 MiB/s; the bucket grants no initial burst.
		{"bandwidth", ratelimit.ParentLimit{BandwidthBytesPerSecond: 20 << 20}, 1700 * time.Millisecond},
		// One probe plus eight ranges at 10 GETs/s, each spaced by 100 ms.
		{"list-qps", ratelimit.ParentLimit{ListQPS: 10}, 850 * time.Millisecond},
		{"unlimited", ratelimit.ParentLimit{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("0123456789abcdef"), 8*partSize/16)
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
			limit := tc.limit
			limit.ParentID, limit.Generation, limit.Revision = 9, 1, 1
			// No task request can be admitted before this instant, so the k-th
			// LIST permit cannot be granted before origin + k*interval.
			origin := time.Now()
			if err := limits.ApplyParent(context.Background(), limit); err != nil {
				t.Fatal(err)
			}
			budget, _ := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
			lender := transferstore.NewPartLender()
			client := &http.Client{Transport: nodes.Transport(server.Client().Transport, nodecontrol.RequestContext{TaskID: 9, Generation: 1, Bucket: "bucket", PathStyle: true})}
			open := func() (*transferstore.S3, error) {
				return transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: partSize, UploadMemory: budget, PartLender: lender, RangedReadMinSize: 1, RetryMaxAttempts: 1, HTTPClient: client})
			}
			factory := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
				source, err := open()
				if err != nil {
					return nil, nil, err
				}
				target, err := open()
				return source, target, err
			})
			repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rates := NewTaskRates()
			engine, err := execution.NewEngine(repo, factory, execution.Options{DecorateReader: rates.Decorator(nodes.Decorator(limits.Decorator))})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close(context.Background())
			lender.SetIdle(engine.IdleObjectSlots)
			job := execution.Job{ChildID: "9-1", ParentID: 9, Number: 1, Generation: 1, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true,
				Source: execution.Endpoint{Kind: "S3", Ref: "src", Bucket: "bucket"}, Destination: execution.Endpoint{Kind: "S3", Ref: "dst", Bucket: "bucket"},
				Items: []execution.Item{{ID: "1", Source: transferstore.Object{Key: "source", Size: int64(len(payload)), ETag: "source-etag"}, DestinationKey: "target"}}}
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
			bucket.mu.Lock()
			defer bucket.mu.Unlock()
			if !bytes.Equal(bucket.target, payload) || len(bucket.gets) != 9 {
				t.Fatalf("target integrity or GET count %d", len(bucket.gets))
			}
			if elapsed < tc.minimum {
				t.Fatalf("ranged copy exceeded the task limit: %v < %v", elapsed, tc.minimum)
			}
			if tc.limit.ListQPS > 0 {
				// Server arrivals trail their client-side permits by a variable
				// delay, so adjacent gaps can shrink on a slow runner. Arrival k
				// is never earlier than permit k, which the limiter spaces one
				// interval apart starting one interval after origin.
				interval := time.Duration(float64(time.Second) / tc.limit.ListQPS)
				gets := slices.Clone(bucket.gets)
				slices.SortFunc(gets, time.Time.Compare)
				for i, at := range gets {
					if since, want := at.Sub(origin), time.Duration(i+1)*interval; since < want {
						t.Fatalf("GET %d arrived %v after origin, want >= %v", i+1, since, want)
					}
					if i > 0 {
						t.Logf("GETs %d and %d were %v apart", i, i+1, at.Sub(gets[i-1]))
					}
				}
			}
			if tc.name == "unlimited" && bucket.maxParts <= 4 {
				t.Fatalf("idle engine did not lend part slots: %d", bucket.maxParts)
			}
			if lender.Lent() != 0 {
				t.Fatal("borrowed slots outlived the copy", lender.Lent())
			}
		})
	}
}
