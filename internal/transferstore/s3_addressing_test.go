package transferstore

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAutoPathStyle(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"":                                      false,
		"https://s3.amazonaws.com":              false,
		"https://s3.us-west-2.amazonaws.com":    false,
		"https://s3.cn-north-1.jdcloud-oss.com": false,
		"https://oss-cn-hangzhou.aliyuncs.com/": false,
		"https://storage.example.com.":          false,
		"https://s3.example.com:443":            true,
		"http://host.docker.internal:18333":     true,
		"http://ot-eval-native-s3:8333":         true,
		"http://10.0.0.5:9000":                  true,
		"http://10.0.0.5":                       true,
		"http://[::1]":                          true,
		"http://[fd00::5]:9000":                 true,
		"http://localhost":                      true,
		"http://LOCALHOST":                      true,
		"http://minio":                          true,
		"http://minio.internal":                 true,
		"http://s3.corp.local":                  true,
		"http://nas.lan":                        true,
		"http://nas.lan.":                       true,
		"https://internal.example.com":          false,
		"https://bucket-local.example.com":      false,
		"https://s3.example.local.example.com":  false,
		"https://s3.example.com/":               false,
		// The host as written: these are not IP addresses for net.ParseIP.
		"http://10.1":        false,
		"http://192.168.1":   false,
		"http://1.2.3.04":    false,
		"http://0x7f.0.0.1":  false,
		"http://10.0.0.5.":   true,
		"http://2130706433":  true,
		"http://user@minio":  true,
		"http://s3.example:": false,
	} {
		if got := AutoPathStyle(endpoint); got != want {
			t.Errorf("AutoPathStyle(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestClassifyStorageMessage(t *testing.T) {
	for message, want := range map[string]StorageErrorKind{
		`operation error S3: ListObjectsV2, exceeded maximum number of attempts, 3, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Get "http://src.host.docker.internal:18333/?list-type=2": dial tcp: lookup src.host.docker.internal on 127.0.0.11:53: no such host`: StorageErrorDNS,
		`operation error S3: ListObjectsV2, exceeded maximum number of attempts, 3, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Get "http://src-bucket.ot-eval-native-s3:8333/?list-type=2&max-keys=1000&prefix=": EOF`:                                             StorageErrorEOF,
		`operation error S3: ListBuckets, https response error StatusCode: 403, RequestID: 1, HostID: , api error InvalidAccessKeyId: The access key ID you provided does not exist in our records. [storage response: http=403]`:                                                                       StorageErrorInvalidAccessKey,
		`operation error S3: ListBuckets, https response error StatusCode: 403, RequestID: 1, HostID: , api error SignatureDoesNotMatch: The request signature we calculated does not match`:                                                                                                            StorageErrorSignatureMismatch,
		`operation error S3: ListObjectsV2, https response error StatusCode: 404, api error NoSuchBucket: The specified bucket does not exist`:                                                                                                                                                          StorageErrorNoSuchBucket,
		`operation error S3: ListBuckets, https response error StatusCode: 403, api error AccessDenied: Access Denied.`:                                                                                                                                                                                 StorageErrorAccessDenied,
		`Get "http://127.0.0.1:9/": dial tcp 127.0.0.1:9: connect: connection refused`:                                                                                                                                                                                                                  StorageErrorConnectionRefused,
		`dial tcp 10.0.0.5:9000: connectex: No connection could be made because the target machine actively refused it.`:                                                                                                                                                                                StorageErrorConnectionRefused,
		`read tcp 10.0.0.2:5555->10.0.0.5:9000: read: connection reset by peer`:                                                                                                                                                                                                                         StorageErrorConnectionReset,
		`dial tcp 10.0.0.5:9000: i/o timeout`: StorageErrorTimeout,
		`net/http: TLS handshake timeout`:     StorageErrorTimeout,
		`context deadline exceeded`:           StorageErrorTimeout,
		`tls: failed to verify certificate: x509: certificate signed by unknown authority`: StorageErrorTLS,
		`x509: certificate is valid for a.example.com, not b.example.com`:                  StorageErrorTLS,
		`lookup s3.example.com: Temporary failure in name resolution`:                      StorageErrorDNS,
		`unexpected EOF`: StorageErrorEOF,
		`multipart parts still present after abort`:                 StorageErrorUnknown,
		`api error InternalError: We encountered an internal error`: StorageErrorUnknown,
		``: StorageErrorUnknown,
	} {
		if got := ClassifyStorageMessage(message); got != want {
			t.Errorf("ClassifyStorageMessage(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestClassifyStorageErrorPrefersTypes(t *testing.T) {
	for _, test := range []struct {
		err  error
		want StorageErrorKind
	}{
		{&net.DNSError{Err: "server misbehaving", Name: "s3.example.com", IsTimeout: true}, StorageErrorTimeout},
		{&net.DNSError{Err: "nope", Name: "s3.example.com", IsNotFound: true}, StorageErrorDNS},
		{fmt.Errorf("wrapped: %w", x509.UnknownAuthorityError{}), StorageErrorTLS},
		{fmt.Errorf("Get: %w", io.EOF), StorageErrorEOF},
		{fmt.Errorf("deadline: %w", context.DeadlineExceeded), StorageErrorTimeout},
		{errors.New("something else"), StorageErrorUnknown},
		{nil, StorageErrorUnknown},
	} {
		if got := ClassifyStorageError(test.err); got != test.want {
			t.Errorf("ClassifyStorageError(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

// checkS3 is a path-style S3 server reached through a fake DNS: host names in
// resolvable go to the server, any other name fails like an unknown bucket
// host name.
type checkS3 struct {
	server     *httptest.Server
	mu         sync.Mutex
	requests   []string
	listDenied bool
	bucketCode string
	buckets    []string
}

func newCheckS3(t *testing.T) *checkS3 {
	c := &checkS3{buckets: []string{"src", "dst"}}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, r.Host+" "+r.URL.Path)
		listDenied, bucketCode, buckets := c.listDenied, c.bucketCode, c.buckets
		c.mu.Unlock()
		if r.URL.Path == "/" {
			if listDenied {
				protocolError(w, 403, "AccessDenied")
				return
			}
			var body strings.Builder
			for _, bucket := range buckets {
				fmt.Fprintf(&body, "<Bucket><Name>%s</Name></Bucket>", bucket)
			}
			fmt.Fprintf(w, "<ListAllMyBucketsResult><Buckets>%s</Buckets></ListAllMyBucketsResult>", body.String())
			return
		}
		if bucketCode != "" {
			protocolError(w, map[string]int{"AccessDenied": 403, "NoSuchBucket": 404}[bucketCode], bucketCode)
			return
		}
		fmt.Fprintf(w, "<ListBucketResult><Name>%s</Name><IsTruncated>false</IsTruncated></ListBucketResult>", strings.Trim(r.URL.Path, "/"))
	}))
	t.Cleanup(c.server.Close)
	return c
}

// client resolves only the given host names.
func (c *checkS3) client(resolvable ...string) *http.Client {
	address := strings.TrimPrefix(c.server.URL, "http://")
	transport := &http.Transport{DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(target)
		for _, name := range resolvable {
			if host == name {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
	}}
	return &http.Client{Transport: transport}
}

func (c *checkS3) requested() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func TestCheckConnectionFindsVirtualHostedBucketThatDoesNotResolve(t *testing.T) {
	s3 := newCheckS3(t)
	config := S3Config{Endpoint: "http://storage.example.com", AccessKey: "test", SecretKey: "test-secret", HTTPClient: s3.client("storage.example.com")}
	config.PathStyle = AutoPathStyle(config.Endpoint)
	check, err := CheckConnection(context.Background(), config, "")
	if err != nil {
		t.Fatal(err)
	}
	if check.PathStyle || strings.Join(check.Buckets, ",") != "src,dst" || check.ListError != "" || check.Bucket != "src" ||
		check.BucketErrorKind != StorageErrorDNS || !strings.Contains(check.BucketError, "src.storage.example.com") ||
		!StorageErrorSuggestsPathStyle(check.BucketErrorKind, check.PathStyle) {
		t.Fatalf("virtual-hosted bucket check: %+v", check)
	}
	config.PathStyle = true
	check, err = CheckConnection(context.Background(), config, "")
	if err != nil || check.BucketError != "" || check.Bucket != "src" || !check.PathStyle {
		t.Fatalf("path-style bucket check: %+v %v", check, err)
	}
	requests := s3.requested()
	if last := requests[len(requests)-1]; last != "storage.example.com /src" {
		t.Fatalf("bucket check was not a path-style request: %q", requests)
	}
}

func TestCheckConnectionWithBucketScopedKeys(t *testing.T) {
	s3 := newCheckS3(t)
	s3.listDenied = true
	config := S3Config{Endpoint: s3.server.URL, AccessKey: "test", SecretKey: "test-secret", PathStyle: true, HTTPClient: s3.server.Client()}
	check, err := CheckConnection(context.Background(), config, "")
	if err != nil || check.ListErrorKind != StorageErrorAccessDenied || check.Bucket != "" || check.BucketError != "" || len(check.Buckets) != 0 {
		t.Fatalf("denied ListBuckets without a bucket: %+v %v", check, err)
	}
	check, err = CheckConnection(context.Background(), config, "only")
	if err != nil || check.ListErrorKind != StorageErrorAccessDenied || check.Bucket != "only" || check.BucketError != "" {
		t.Fatalf("denied ListBuckets with a bucket: %+v %v", check, err)
	}
	s3.mu.Lock()
	s3.bucketCode = "NoSuchBucket"
	s3.mu.Unlock()
	check, err = CheckConnection(context.Background(), config, "missing")
	if err != nil || check.BucketErrorKind != StorageErrorNoSuchBucket {
		t.Fatalf("missing bucket: %+v %v", check, err)
	}
}

func TestCheckConnectionStopsAfterFailedListing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	_ = listener.Close()
	config := S3Config{Endpoint: endpoint, AccessKey: "test", SecretKey: "test-secret", PathStyle: true}
	check, err := CheckConnection(context.Background(), config, "bucket")
	if err != nil || check.ListErrorKind != StorageErrorConnectionRefused || check.Bucket != "" {
		t.Fatalf("refused connection: %+v %v", check, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CheckConnection(ctx, config, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled check must return the cancellation: %v", err)
	}
}
