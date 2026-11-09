package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestStorageCheckUsesTheEffectiveAddressing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>src</Name></Bucket></Buckets></ListAllMyBucketsResult>")
			return
		}
		fmt.Fprint(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	// Only storage.example.com resolves, as for self-hosted S3 without
	// wildcard DNS.
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(target); host != "storage.example.com" {
			return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}}
	service := &Service{Factory: &storageconfig.Factory{HTTPClientForEndpoint: func(context.Context, execution.Endpoint, storageconfig.Config) *http.Client { return client }}}
	check := func(endpoint string, pathStyle *bool) transferstore.ConnectionCheck {
		t.Helper()
		payload, _ := json.Marshal(StorageCheckRequest{Config: storageconfig.Config{Alias: "s3", Endpoint: endpoint, AccessKey: "key", SecretKey: "secret", PathStyleEnable: pathStyle}})
		response, err := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "storage_check", Payload: payload}})
		var result transferstore.ConnectionCheck
		if err != nil || response.Status != 200 || json.Unmarshal(response.Payload, &result) != nil {
			t.Fatalf("storage_check %s: %+v %v", endpoint, response, err)
		}
		return result
	}
	// A DNS name without a port stays virtual-hosted: the bucket host name
	// does not resolve, and the check reports it.
	if result := check("http://storage.example.com", nil); result.PathStyle || result.Bucket != "src" || result.BucketErrorKind != transferstore.StorageErrorDNS {
		t.Fatalf("virtual-hosted check: %+v", result)
	}
	// An explicit port selects path-style automatically.
	if result := check("http://storage.example.com:80", nil); !result.PathStyle || result.Bucket != "src" || result.BucketError != "" {
		t.Fatalf("automatic path-style check: %+v", result)
	}
	// An explicit choice is kept.
	on := true
	if result := check("http://storage.example.com", &on); !result.PathStyle || result.BucketError != "" {
		t.Fatalf("explicit path-style check: %+v", result)
	}
	payload, _ := json.Marshal(map[string]any{"config": map[string]any{"alias": "s3"}, "unknown": true})
	if response, _ := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "storage_check", Payload: payload}}); response.Status != 400 {
		t.Fatalf("malformed request: %+v", response)
	}
}
