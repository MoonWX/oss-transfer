package nodecontrol

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
)

func TestTaskRequestRatesClassifyAdmittedAttemptsPerGeneration(t *testing.T) {
	f := newFixture(t, Options{UseZeroDefaults: true, ListQPS: 50, HeadQPS: 70})
	must(t, f.limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: 7, Generation: 2, Revision: 1}))
	now := time.Unix(1000, 0)
	var clock sync.Mutex
	f.controller.requests.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	advance := func(d time.Duration) { clock.Lock(); now = now.Add(d); clock.Unlock() }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client := &http.Client{Transport: f.controller.Transport(nil, RequestContext{Bucket: "bucket", PathStyle: true})}
	send := func(ctx context.Context, method, path string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
		res, err := client.Do(req)
		must(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	tagged := WithRequestContext(context.Background(), RequestContext{TaskID: 7, Generation: 2})
	for _, request := range []struct{ method, path string }{
		{"GET", "/bucket?list-type=2&prefix=a"}, // ListObjects
		{"GET", "/bucket/a"},                    // GetObject
		{"GET", "/bucket/b"},
		{"GET", "/bucket/a?uploadId=u&max-parts=1"}, // ListParts counts as LIST
		{"HEAD", "/bucket/a"},
		{"PUT", "/bucket/a"}, // PutObject
		{"PUT", "/bucket/a?partNumber=1&uploadId=u"},
		{"POST", "/bucket/a?uploads"},      // CreateMultipartUpload
		{"POST", "/bucket/a?uploadId=u"},   // CompleteMultipartUpload
		{"DELETE", "/bucket/a?uploadId=u"}, // AbortMultipartUpload is not counted
	} {
		send(tagged, request.method, request.path)
	}
	send(context.Background(), "GET", "/bucket/untagged")
	// A restarted generation keeps the parent but must not inherit its rates.
	must(t, f.limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: 7, Generation: 3, Revision: 2}))
	send(WithRequestContext(context.Background(), RequestContext{TaskID: 7, Generation: 3}), "PUT", "/bucket/next")
	if rates := f.controller.TaskRequests(7, 2).Rates; rates != (TaskRequestRates{}) {
		t.Fatalf("incomplete second counted: %+v", rates)
	}
	advance(time.Second)
	got := f.controller.TaskRequests(7, 2)
	if want := (TaskRequestRates{List: 0.4, Get: 0.4, Head: 0.2, Put: 0.8}); got.Rates != want {
		t.Fatalf("rates = %+v, want %+v", got.Rates, want)
	}
	if got.NodeLimits != (NodeRequestLimits{List: 50, Head: 70}) {
		t.Fatalf("node limits = %+v", got.NodeLimits)
	}
	if rates := f.controller.TaskRequests(7, 3).Rates; rates != (TaskRequestRates{Put: 0.2}) {
		t.Fatalf("generations mixed: %+v", rates)
	}
	if rates := f.controller.TaskRequests(8, 2).Rates; rates != (TaskRequestRates{}) {
		t.Fatalf("tasks mixed: %+v", rates)
	}
	advance(4 * time.Second)
	if rates := f.controller.TaskRequests(7, 2).Rates; rates.Put != 0.8 {
		t.Fatalf("sample left the window early: %+v", rates)
	}
	advance(time.Second)
	if rates := f.controller.TaskRequests(7, 2).Rates; rates != (TaskRequestRates{}) {
		t.Fatalf("old sample survived: %+v", rates)
	}
	advance(2 * time.Second)
	f.controller.TaskRequests(1, 1)
	f.controller.requests.mu.Lock()
	remaining := len(f.controller.requests.series)
	f.controller.requests.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("idle task counters were not pruned: %d", remaining)
	}
}
