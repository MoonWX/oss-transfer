package agentservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

func TestAgentHTTPPoolHonorsCustomClient(t *testing.T) {
	custom := &http.Client{Timeout: 7 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	factory := &storageconfig.Factory{HTTPClient: custom}
	configureNodeFactory(factory, nil)
	if factory.HTTPClient != custom || factory.HTTPClient.Timeout != 7*time.Second {
		t.Fatal("custom client replaced")
	}
	factory = &storageconfig.Factory{}
	configureNodeFactory(factory, nil)
	pool, ok := factory.HTTPClient.Transport.(*http.Transport)
	if !ok || pool == http.DefaultTransport || pool.MaxIdleConnsPerHost != 256 || pool.MaxIdleConns != 1024 {
		t.Fatal("missing bounded shared idle pool")
	}
}

// Measures actual TLS connections between bursts through the Agent rate-control
// wrapper. The endpoint echoes payloads; this is not a cloud throughput claim.
func TestAgentHTTPPoolPerformance(t *testing.T) {
	if os.Getenv("OSS_TRANSFER_PERFORMANCE") == "" {
		t.Skip("set OSS_TRANSFER_PERFORMANCE=1")
	}
	const concurrency, waves = 16, 8
	data := bytes.Repeat([]byte("payload-0123456789"), 1024)
	expected := sha256.Sum256(data)
	for _, mode := range []string{"old_idle_2", "shared_idle_256"} {
		t.Run(mode, func(t *testing.T) {
			var connections atomic.Int64
			var arrivals atomic.Int64
			var wave sync.Mutex
			barrier := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || sha256.Sum256(body) != expected {
					t.Errorf("request content mismatch: %v", err)
					w.WriteHeader(500)
					return
				}
				wave.Lock()
				done := barrier
				if arrivals.Add(1)%concurrency == 0 {
					close(done)
					barrier = make(chan struct{})
				}
				wave.Unlock()
				select {
				case <-done:
				case <-r.Context().Done():
					return
				}
				w.Write(body)
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
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
			nodes, err := nodecontrol.New(db, limits, nodecontrol.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer nodes.Close()
			factory := &storageconfig.Factory{}
			if mode == "old_idle_2" {
				factory.HTTPClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
			}
			configureNodeFactory(factory, nodes)
			pool := factory.HTTPClient.Transport.(*http.Transport)
			pool.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fixture certificate
			defer pool.CloseIdleConnections()
			start := time.Now()
			for range waves {
				var workers sync.WaitGroup
				for range concurrency {
					workers.Add(1)
					go func() {
						defer workers.Done()
						// Every object creates a fresh endpoint wrapper as a new child can, while
						// the underlying pool must survive across those clients.
						client := factory.HTTPClientForEndpoint(context.Background(), execution.Endpoint{Ref: "fixture", Bucket: "bucket"}, storageconfig.Config{})
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						req, err := http.NewRequestWithContext(ctx, "PUT", server.URL+"/key", bytes.NewReader(data))
						if err != nil {
							t.Error(err)
							return
						}
						response, err := client.Do(req)
						if err != nil {
							t.Error(err)
							return
						}
						got, err := io.ReadAll(response.Body)
						response.Body.Close()
						if err != nil || response.StatusCode != 200 || sha256.Sum256(got) != expected {
							t.Errorf("response mismatch: %v status %d", err, response.StatusCode)
						}
					}()
				}
				workers.Wait()
				// Let all bodies return to the idle pool between synchronized bursts.
				time.Sleep(5 * time.Millisecond)
			}
			t.Logf("mode=%s requests=%d connections=%d elapsed=%s sha256=all_match", mode, concurrency*waves, connections.Load(), time.Since(start))
			if mode == "shared_idle_256" && connections.Load() > concurrency {
				t.Fatal(fmt.Sprintf("connections not reused: %d", connections.Load()))
			}
		})
	}
}
