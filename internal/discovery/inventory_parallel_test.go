package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func metadataTestSource(t testing.TB, handler http.HandlerFunc) *transferstore.S3 {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	source, err := transferstore.NewS3(transferstore.S3Config{Endpoint: server.URL, Region: "test-region", Bucket: "inventory", AccessKey: "test", SecretKey: "test", PathStyle: true, RetryMaxAttempts: 1, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func metadataKeys() []string {
	keys := make([]string, 128)
	for i := range keys {
		keys[i] = fmt.Sprintf("%03d", i)
	}
	return keys
}

func metadataResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "73")
	w.Header().Set("ETag", `"pinned-source"`)
	w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
	w.WriteHeader(http.StatusOK)
}

func TestInventoryMetadataOverlapsHEADButEmitsOriginalOrderAndMissingKeys(t *testing.T) {
	var active, peak, total atomic.Int32
	started := make(chan struct{})
	source := metadataTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || !strings.HasPrefix(r.URL.Path, "/inventory/") {
			t.Errorf("inventory made a payload or list request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		if total.Add(1) == inventoryMetadataConcurrency {
			close(started)
		}
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		if strings.HasSuffix(r.URL.Path, "/007") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		metadataResponse(w)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keys := metadataKeys()
	index := 0
	err := scanInventoryPage(ctx, source, keys, inventoryMetadataConcurrency, func(object transferstore.Object, missing bool) error {
		if object.Key != keys[index] || missing != (index == 7) || (!missing && (object.Size != 73 || !transferstore.EqualETag(object.ETag, "pinned-source"))) {
			t.Errorf("ordered metadata %d: %+v missing=%v", index, object, missing)
		}
		index++
		return nil
	})
	if err != nil || index != len(keys) || total.Load() != 128 || peak.Load() < 2 || peak.Load() > inventoryMetadataConcurrency {
		t.Fatalf("inventory was not bounded, complete, concurrent: emitted=%d total=%d peak=%d err=%v", index, total.Load(), peak.Load(), err)
	}
}

func TestInventoryMetadataCallbackFailureCancelsAndJoinsOutstandingHEADs(t *testing.T) {
	var startedCount, active atomic.Int32
	started := make(chan struct{})
	source := metadataTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		if startedCount.Add(1) == inventoryMetadataConcurrency {
			close(started)
		}
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/inventory/000" {
			metadataResponse(w)
			return
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := errors.New("consumer stopped")
	calls := 0
	err := scanInventoryPage(ctx, source, metadataKeys(), inventoryMetadataConcurrency, func(object transferstore.Object, missing bool) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("consumer error lost: calls=%d err=%v", calls, err)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 || startedCount.Load() > inventoryMetadataConcurrency+1 {
		t.Fatalf("HEADs survived cancellation or read beyond the bounded workers: active=%d started=%d", active.Load(), startedCount.Load())
	}
}

func TestInventoryMetadataFailureDoesNotEmitLaterKeys(t *testing.T) {
	source := metadataTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inventory/003" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		metadataResponse(w)
	})
	var emitted []string
	err := scanInventoryPage(context.Background(), source, metadataKeys(), inventoryMetadataConcurrency, func(object transferstore.Object, missing bool) error {
		emitted = append(emitted, object.Key)
		return nil
	})
	if err == nil || strings.Join(emitted, ",") != "000,001,002" {
		t.Fatalf("out-of-order failure: %v %v", emitted, err)
	}
}

// A real HTTP/S3 client with controlled 10 ms HEAD latency isolates the pipeline
// improvement. It does not measure JD OSS throughput or object body transfer.
func BenchmarkInventoryMetadataHTTP(b *testing.B) {
	for _, workers := range []int{1, inventoryMetadataConcurrency} {
		b.Run(fmt.Sprint(workers), func(b *testing.B) {
			source := metadataTestSource(b, func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(10 * time.Millisecond)
				metadataResponse(w)
			})
			keys := metadataKeys()
			b.ResetTimer()
			for range b.N {
				count := 0
				if err := scanInventoryPage(context.Background(), source, keys, workers, func(object transferstore.Object, _ bool) error {
					if object.Key != keys[count] {
						b.Fatal("metadata order changed")
					}
					count++
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if count != len(keys) {
					b.Fatal("metadata missing")
				}
			}
			b.ReportMetric(float64(len(keys)), "objects/op")
		})
	}
}
