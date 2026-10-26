package agentservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestAgentJDOSSProtocolSelectionPreservesAdmissionAndSharedPools(t *testing.T) {
	payload := []byte("conditional upload through node admission")
	var mu sync.Mutex
	protocols := map[string]int{}
	connections := map[string]map[string]bool{}
	requests := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protocols[r.Host] = r.ProtoMajor
		if connections[r.Host] == nil {
			connections[r.Host] = map[string]bool{}
		}
		connections[r.Host][r.RemoteAddr] = true
		requests++
		mu.Unlock()
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) || r.Header.Get("If-None-Match") != "*" {
			t.Errorf("upload changed: body=%q conditional=%q err=%v", got, r.Header.Get("If-None-Match"), err)
		}
		w.Header().Set("ETag", `"uploaded"`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	pool := server.Client().Transport.(*http.Transport).Clone()
	pool.Proxy = nil
	pool.ForceAttemptHTTP2 = true
	pool.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // local fixture certificate with simulated provider names
	pool.MaxIdleConns, pool.MaxIdleConnsPerHost = 1024, 256
	pool.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer pool.CloseIdleConnections()
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
	parent := ratelimit.ParentLimit{ParentID: 7, Generation: 1, Revision: 1}
	if err := limits.ApplyParent(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	factory := &storageconfig.Factory{HTTPClient: &http.Client{Transport: pool, Timeout: 5 * time.Second}}
	configureNodeFactory(factory, nodes)
	ctx := nodecontrol.WithRequestContext(context.Background(), nodecontrol.RequestContext{TaskID: 7, Generation: 1})
	var store *transferstore.S3
	var jdStore *transferstore.S3
	for range 2 {
		for host, wantProtocol := range map[string]int{"s3-internal.cn-north-1.jdcloud-oss.com": 1, "storage.example.org": 2, "s3.jdcloud-oss.com.example.org": 2} {
			config := storageconfig.Config{Endpoint: "https://" + host, Alias: "fixture", AccessKey: "access", SecretKey: "secret", PathStyleEnable: func() *bool { b := true; return &b }()}
			client := factory.HTTPClientForEndpoint(ctx, execution.Endpoint{Kind: "S3", Ref: "fixture", Bucket: "bucket"}, config)
			if client.Timeout != 5*time.Second {
				t.Fatal("custom client timeout lost")
			}
			settings, err := config.S3Config("bucket")
			if err != nil {
				t.Fatal(err)
			}
			settings.HTTPClient, settings.RetryMaxAttempts = client, 1
			store, err = transferstore.NewS3(settings)
			if err != nil {
				t.Fatal(err)
			}
			if wantProtocol == 1 {
				jdStore = store
			}
			result, err := store.Write(ctx, "object", bytes.NewReader(payload), transferstore.WriteOptions{Size: int64(len(payload))})
			if err != nil || !result.Committed {
				t.Fatalf("%s: result=%+v err=%v", host, result, err)
			}
			mu.Lock()
			protocol := protocols[host]
			count := len(connections[host])
			mu.Unlock()
			if protocol != wantProtocol || count != 1 {
				t.Fatalf("%s: protocol=%d connections=%d", host, protocol, count)
			}
		}
	}
	mu.Lock()
	before := requests
	mu.Unlock()
	if before != 12 {
		t.Fatalf("HEAD/PUT request count=%d", before)
	}
	store = jdStore
	if err := nodes.SetRate(ctx, "put", .01); err != nil {
		t.Fatal(err)
	}
	// Consume the initial permit; the next PUT must wait for the node cap.
	admission, stopAdmission := context.WithTimeout(ctx, time.Second)
	err = nodes.BeforeRequest(admission, nodecontrol.RequestContext{TaskID: 7, Generation: 1}, "put", "put")
	stopAdmission()
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = store.Write(limited, "object", bytes.NewReader(payload), transferstore.WriteOptions{Size: int64(len(payload)), Overwrite: true})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("node request limit bypassed: %v", err)
	}
	if err := nodes.SetRate(ctx, "put", 0); err != nil {
		t.Fatal(err)
	}
	parent.Revision, parent.Stopped = 2, true
	if err := limits.ApplyParent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	_, err = store.Write(ctx, "object", bytes.NewReader(payload), transferstore.WriteOptions{Size: int64(len(payload)), Overwrite: true})
	if !errors.Is(err, ratelimit.ErrStopped) {
		t.Fatalf("task request admission bypassed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != before {
		t.Fatalf("blocked requests reached storage: before=%d after=%d", before, requests)
	}
}
