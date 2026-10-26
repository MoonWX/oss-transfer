package transferstore

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
)

func TestJDOSSHTTPSDomainMatch(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		want     bool
	}{
		{"https://s3.cn-north-1.jdcloud-oss.com", true},
		{"https://s3-internal.cn-north-1.jdcloud-oss.com:443", true},
		{"https://bucket.s3.cn-north-1.jdcloud-oss.com", true},
		{"https://S3.CN-NORTH-1.JDCLOUD-OSS.COM.", true},
		{"http://s3.cn-north-1.jdcloud-oss.com", false},
		{"https://jdcloud-oss.com", false},
		{"https://notjdcloud-oss.com", false},
		{"https://s3.cn-north-1.jdcloud-oss.com.example.org", false},
		{"https://s3..jdcloud-oss.com", false},
		{"https://-.jdcloud-oss.com", false},
		{"https://127.0.0.1", false},
		{"https://s3.amazonaws.com", false},
	} {
		endpoint, err := url.Parse(test.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if got := jdOSSHTTPS(endpoint); got != test.want {
			t.Errorf("%s matched=%v want=%v", test.endpoint, got, test.want)
		}
	}
}

func TestS3ProtocolPoolRetainsSettingsWithoutChangingSharedTransport(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns, base.MaxIdleConnsPerHost, base.MaxConnsPerHost = 1024, 256, 77
	base.ResponseHeaderTimeout = 19 * time.Second
	base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "storage.example", NextProtos: []string{"h2", "http/1.1"}}
	base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{"h2": func(string, *tls.Conn) http.RoundTripper { return nil }}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	base.Protocols = protocols
	selected := S3CompatibleTransport(base).(*s3ProtocolTransport)
	if selected.base != base || selected.http1 == base || selected.http1.TLSClientConfig == base.TLSClientConfig || selected.http1.Protocols == base.Protocols {
		t.Fatal("compatibility transport changed or reused mutable shared settings")
	}
	pool := selected.http1
	if pool.MaxIdleConns != 1024 || pool.MaxIdleConnsPerHost != 256 || pool.MaxConnsPerHost != 77 || pool.ResponseHeaderTimeout != 19*time.Second || pool.TLSClientConfig.ServerName != "storage.example" || pool.TLSClientConfig.MinVersion != tls.VersionTLS12 || pool.Proxy == nil || pool.DialContext == nil {
		t.Fatal("compatibility pool lost connection, TLS, proxy or timeout settings")
	}
	if pool.ForceAttemptHTTP2 || pool.Protocols.HTTP2() || !pool.Protocols.HTTP1() || len(pool.TLSNextProto) != 0 || !reflect.DeepEqual(pool.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatal("HTTP/2 remains enabled or can be negotiated through inherited ALPN")
	}
	if !base.ForceAttemptHTTP2 || !base.Protocols.HTTP2() || len(base.TLSNextProto) != 1 || !reflect.DeepEqual(base.TLSClientConfig.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatal("shared transport was changed")
	}
	if S3CompatibleTransport(selected) != selected {
		t.Fatal("existing wrapper was replaced")
	}
}

func TestDefaultS3ClientUsesHTTP1OnlyForJDOSS(t *testing.T) {
	for _, endpoint := range []string{"https://s3.cn-north-1.jdcloud-oss.com", "https://s3.example.org"} {
		store, err := NewS3(S3Config{Endpoint: endpoint, Bucket: "bucket", AccessKey: "key", SecretKey: "secret"})
		if err != nil {
			t.Fatal(err)
		}
		client := store.client.Options().HTTPClient.(*s3DiagnosticClient).next.(*awshttp.BuildableClient)
		pool := client.GetTransport()
		wantHTTP1 := endpoint == "https://s3.cn-north-1.jdcloud-oss.com"
		if wantHTTP1 && (pool.Protocols == nil || pool.Protocols.HTTP2() || !pool.Protocols.HTTP1() || pool.ForceAttemptHTTP2 || len(pool.TLSNextProto) != 0 || !reflect.DeepEqual(pool.TLSClientConfig.NextProtos, []string{"http/1.1"})) {
			t.Fatal("default SDK JDCloud client did not disable h2")
		}
		if !wantHTTP1 && !pool.ForceAttemptHTTP2 {
			t.Fatal("non-JDCloud SDK transport was changed")
		}
	}
}
