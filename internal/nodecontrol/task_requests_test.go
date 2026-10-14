package nodecontrol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
)

// Count actual HTTP attempts and bound their total rate after transport
// admission. Individual server arrival times can bunch after scheduling or
// network delays; joint permit atomicity is tested in ratelimit/requests_test.go.
func TestTaskRequestsAcrossChildrenAgentsAndNodeBudgets(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		agents, parents, requests int
		taskRate, nodeRate        float64
		minimum                   time.Duration
	}{
		{"children-share-parent", 1, 1, 12, 20, 0, 550 * time.Millisecond},
		{"two-agent-cluster-shares", 2, 1, 6, 10, 0, 550 * time.Millisecond},
		{"two-parents-share-node", 1, 2, 6, 100, 20, 520 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var received []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				received = append(received, time.Now())
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			clients := make([]*http.Client, tc.agents)
			for a := range clients {
				f := newFixture(t, Options{UseZeroDefaults: true, PutQPS: tc.nodeRate})
				for p := 1; p <= tc.parents; p++ {
					must(t, f.limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: int64(p), Generation: 1, Revision: 1, PutQPS: tc.taskRate}))
				}
				clients[a] = &http.Client{Transport: f.controller.Transport(nil, RequestContext{Bucket: "bucket", PathStyle: true})}
			}
			start := time.Now()
			var group sync.WaitGroup
			failures := make(chan error, tc.agents*tc.parents*tc.requests)
			for _, client := range clients {
				for p := 1; p <= tc.parents; p++ {
					for n := 0; n < tc.requests; n++ {
						group.Add(1)
						go func(client *http.Client, parent int64) {
							defer group.Done()
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							ctx = WithRequestContext(ctx, RequestContext{TaskID: parent, Generation: 1})
							req, _ := http.NewRequestWithContext(ctx, "PUT", server.URL+"/bucket/object", nil)
							res, err := client.Do(req)
							if err == nil {
								_, err = io.Copy(io.Discard, res.Body)
								res.Body.Close()
							}
							failures <- err
						}(client, int64(p))
					}
				}
			}
			group.Wait()
			close(failures)
			for err := range failures {
				must(t, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(received) != tc.agents*tc.parents*tc.requests {
				t.Fatal("missing actual HTTP requests", len(received))
			}
			sort.Slice(received, func(i, j int) bool { return received[i].Before(received[j]) })
			if elapsed := received[len(received)-1].Sub(start); elapsed < tc.minimum {
				t.Fatalf("actual requests exceeded allocated rates: %s < %s", elapsed, tc.minimum)
			}
		})
	}
}

func TestTaskReadAndListShareBudgetPersistAndStopFence(t *testing.T) {
	f := newFixture(t, Options{UseZeroDefaults: true})
	policy := ratelimit.ParentLimit{ParentID: 8, Generation: 1, Revision: 1, ListQPS: 10, HeadQPS: 17, PutQPS: 19}
	must(t, f.limits.ApplyParent(context.Background(), policy))
	f.close()
	f.open(t)
	conflict := policy
	conflict.ListQPS = 1000
	if err := f.limits.ApplyParent(context.Background(), conflict); !errors.Is(err, ratelimit.ErrConflict) {
		t.Fatal("persisted caps changed at same revision", err)
	}
	var mu sync.Mutex
	var received []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, time.Now())
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer server.Close()
	client := &http.Client{Transport: f.controller.Transport(nil, RequestContext{Bucket: "bucket", PathStyle: true, TaskID: 8, Generation: 1})}
	start := time.Now()
	for _, path := range []string{"/bucket?list-type=2", "/bucket/object", "/bucket/object-two"} {
		res, err := client.Get(server.URL + path)
		must(t, err)
		res.Body.Close()
	}
	if time.Since(start) < 280*time.Millisecond {
		t.Fatal("object GET bypassed persisted task LIST budget")
	}
	headStart := time.Now()
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("HEAD", server.URL+"/bucket/object", nil)
		res, err := client.Do(req)
		must(t, err)
		res.Body.Close()
	}
	if time.Since(headStart) < 160*time.Millisecond {
		t.Fatal("HEAD requests bypassed persisted parent cap")
	}
	policy.Revision = 2
	policy.ListQPS = .01
	must(t, f.limits.ApplyParent(context.Background(), policy))
	result := make(chan error, 1)
	go func() {
		res, err := client.Get(server.URL + "/bucket/never")
		if res != nil {
			res.Body.Close()
		}
		result <- err
	}()
	time.Sleep(30 * time.Millisecond)
	policy.Revision = 3
	policy.Stopped = true
	must(t, f.limits.ApplyParent(context.Background(), policy))
	select {
	case err := <-result:
		if !errors.Is(err, ratelimit.ErrStopped) {
			t.Fatal("pending HTTP request did not stop", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not wake pending HTTP request")
	}
	policy.Generation = 2
	policy.Revision = 1
	policy.Stopped = false
	policy.ListQPS = 100
	must(t, f.limits.ApplyParent(context.Background(), policy))
	if res, err := client.Get(server.URL + "/bucket/old-generation"); !errors.Is(err, ratelimit.ErrStale) {
		if res != nil {
			res.Body.Close()
		}
		t.Fatal("stale generation sent HTTP request", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 6 {
		t.Fatal("stopped or stale request reached storage", len(received))
	}
}
