package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// storageCheckS3 is a path-style S3 server. Its fake DNS resolves only
// storage.example.com, so a virtual-hosted bucket host name fails like it
// does for self-hosted S3 without wildcard DNS.
type storageCheckS3 struct {
	server *httptest.Server
	mu     sync.Mutex
	// listCode and bucketCode answer ListBuckets and bucket requests with
	// an S3 error code instead of a listing.
	listCode, bucketCode string
}

func newStorageCheckS3(t *testing.T) *storageCheckS3 {
	s := &storageCheckS3{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		listCode, bucketCode := s.listCode, s.bucketCode
		s.mu.Unlock()
		code := bucketCode
		if r.URL.Path == "/" {
			code = listCode
		}
		if code != "" {
			w.Header().Set("Content-Type", "application/xml")
			status := http.StatusForbidden
			if code == "NoSuchBucket" {
				status = http.StatusNotFound
			}
			w.WriteHeader(status)
			fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fixture %s for secret-fixture-key</Message></Error>", code, code)
			return
		}
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>src</Name></Bucket></Buckets></ListAllMyBucketsResult>")
			return
		}
		fmt.Fprint(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *storageCheckS3) set(listCode, bucketCode string) {
	s.mu.Lock()
	s.listCode, s.bucketCode = listCode, bucketCode
	s.mu.Unlock()
}

func (s *storageCheckS3) client() *http.Client {
	address := strings.TrimPrefix(s.server.URL, "http://")
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(target)
		if host == "storage.example.com" || host == "127.0.0.1" {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
	}}}
}

// storageCheckAgent answers the storage operations the way the Agent does,
// with the real connection check. A legacy one predates storage_check.
type storageCheckAgent struct {
	s3     *storageCheckS3
	legacy bool
}

func (a *storageCheckAgent) ReplaySafe(string) bool { return true }

func (a *storageCheckAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := json.Unmarshal(c.Payload, &snapshot); err != nil {
			return agenttransport.Response{}, err
		}
		return fakeReply(200, map[string]any{"revision": snapshot.Revision})
	case "storage_check":
		if a.legacy {
			return fakeUnknownOperation()
		}
		var request struct {
			Config storageconfig.Config `json:"config"`
			Bucket string               `json:"bucket"`
		}
		if err := fakeDecode(c.Payload, &request); err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		settings, err := request.Config.S3Config("validation")
		if err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		settings.HTTPClient = a.s3.client()
		check, err := transferstore.CheckConnection(ctx, settings, request.Bucket)
		if err != nil {
			return fakeReply(500, map[string]string{"message": err.Error()})
		}
		return fakeReply(200, check)
	case "storage_permissions":
		if a.legacy {
			return fakeUnknownOperation()
		}
		var request struct {
			Config storageconfig.Config `json:"config"`
			Bucket string               `json:"bucket"`
			Write  bool                 `json:"write"`
		}
		if err := fakeDecode(c.Payload, &request); err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		// Read access, PUT denied, as read-only keys give.
		check := transferstore.PermissionCheck{Bucket: request.Bucket, PathStyle: true, Write: request.Write, Results: []transferstore.PermissionResult{
			{Operation: transferstore.PermissionList, Status: transferstore.PermissionPassed},
			{Operation: transferstore.PermissionHead, Status: transferstore.PermissionPassed, Key: "a.jpg"},
			{Operation: transferstore.PermissionGet, Status: transferstore.PermissionPassed, Key: "a.jpg"},
			{Operation: transferstore.PermissionPut, Status: transferstore.PermissionFailed, Key: transferstore.PermissionProbePrefix + "x", Error: "api error AccessDenied: denied for secret-fixture-key", ErrorKind: transferstore.StorageErrorAccessDenied},
		}}
		return fakeReply(200, check)
	case "storage_buckets":
		var config storageconfig.Config
		if err := fakeDecode(c.Payload, &config); err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		settings, err := config.S3Config("validation")
		if err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		settings.HTTPClient = a.s3.client()
		buckets, err := transferstore.ListBuckets(ctx, settings)
		if err != nil {
			return fakeReply(500, map[string]string{"code": "operation_failed", "message": err.Error()})
		}
		return fakeReply(200, map[string]any{"buckets": buckets})
	}
	return fakeUnknownOperation()
}

func storageCheckConfig(endpoint string, pathStyle *bool) map[string]any {
	return map[string]any{"alias": "selfhosted", "endpoint": endpoint, "accessKey": "secret-fixture-key", "secretKey": "fixture-secret", "validate": true, "pathStyleEnable": pathStyle}
}

func TestStorageCheckFindsAddressingThatTransfersCannotUse(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	comparisonTestNode(t, f, "storage-check", &storageCheckAgent{s3: s3})
	url := f.http.URL + "/api/master/s3-config"

	// Virtual-hosted (explicit) on a self-hosted endpoint: ListBuckets works,
	// the bucket host name does not resolve. The save is refused with the
	// path-style advice first and the raw error as detail.
	virtual := false
	for locale, want := range map[string]string{
		"zh-CN": "存储验证失败：无法解析存储桶域名 src.storage.example.com。自建 S3（MinIO、SeaweedFS、Ceph 等）需要 Path-style：请把寻址方式改为 Path-style。",
		"en-US": "Storage validation failed: The bucket host name src.storage.example.com cannot be resolved. Self-hosted S3 (MinIO, SeaweedFS, Ceph) needs path-style addressing: set Addressing to Path-style.",
		"ja-JP": "ストレージの検証に失敗しました：バケットのホスト名 src.storage.example.com を解決できません。セルフホストの S3（MinIO、SeaweedFS、Ceph など）にはパス形式が必要です。アドレス指定を Path-style にしてください。",
	} {
		code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, locale, storageCheckConfig("http://storage.example.com", &virtual))
		if err != nil || code != 400 || body["message"] != want || body["reasonCode"] != "STORAGE_DNS" || body["pathStyleHint"] != true ||
			!strings.Contains(fmt.Sprint(body["detail"]), "no such host") {
			t.Fatalf("%s: HTTP %d %v %v", locale, code, body, err)
		}
	}
	snapshot, err := storageconfig.NewMaster(f.db).Snapshot()
	if err != nil || len(snapshot.Connections) != 0 {
		t.Fatalf("a failed check saved the connection: %+v %v", snapshot, err)
	}

	// Automatic addressing on an endpoint with a port is path-style, so the
	// same server passes, and the listing reports the addressing it used.
	code, body := request(t, f.http.Client(), "POST", url, f.admin, storageCheckConfig("http://storage.example.com:9000", nil))
	if code != 200 || body["warning"] != nil {
		t.Fatalf("automatic path-style save: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url, f.admin, nil)
	configs, _ := body["configs"].([]any)
	if code != 200 || len(configs) != 1 || configs[0].(map[string]any)["pathStyleEffective"] != true || configs[0].(map[string]any)["pathStyleEnable"] != nil {
		t.Fatalf("listing: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets", f.admin, nil)
	if code != 200 || body["checkedBucket"] != "src" || body["pathStyle"] != true || body["pathStyleAuto"] != true || fmt.Sprint(body["buckets"]) != "[src]" {
		t.Fatalf("test: HTTP %d %v", code, body)
	}
}

func TestStorageCheckWarnsForBucketScopedKeys(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	comparisonTestNode(t, f, "storage-scoped", &storageCheckAgent{s3: s3})
	url := f.http.URL + "/api/master/s3-config"
	s3.set("AccessDenied", "")
	code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", storageCheckConfig("http://storage.example.com:9000", nil))
	if err != nil || code != 200 || body["warningCode"] != "STORAGE_ACCESS_DENIED" ||
		!strings.HasPrefix(fmt.Sprint(body["warning"]), "该凭据无权列举存储桶（ListBuckets 被拒绝）") || strings.Contains(fmt.Sprint(body["warningDetail"]), "secret-fixture-key") {
		t.Fatalf("bucket-scoped save: HTTP %d %v %v", code, body, err)
	}
	// Naming the bucket checks it instead; nothing is left to warn about.
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=src", f.admin, nil)
	if code != 200 || body["checkedBucket"] != "src" || body["warning"] != nil {
		t.Fatalf("bucket-scoped test: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=a/b", f.admin, nil)
	if code != 400 {
		t.Fatalf("invalid bucket: HTTP %d %v", code, body)
	}
	s3.set("AccessDenied", "NoSuchBucket")
	code, _, body, err = localizedRequest(f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=src", f.admin, "zh-CN", nil)
	if err != nil || code != 502 || body["message"] != "存储桶 src 不存在，请检查存储桶名称和 Endpoint。" || body["reasonCode"] != "STORAGE_NO_SUCH_BUCKET" {
		t.Fatalf("missing bucket: HTTP %d %v %v", code, body, err)
	}
	// Wrong keys block the save with a short sentence; the provider's text,
	// which may repeat the key, is redacted in the detail.
	s3.set("InvalidAccessKeyId", "")
	code, _, body, err = localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", storageCheckConfig("http://storage.example.com:9000", nil))
	if err != nil || code != 400 || body["message"] != "存储验证失败：Access Key 不存在，请检查 Access Key。" ||
		!strings.Contains(fmt.Sprint(body["detail"]), "InvalidAccessKeyId") || strings.Contains(fmt.Sprint(body["detail"]), "secret-fixture-key") {
		t.Fatalf("invalid key: HTTP %d %v %v", code, body, err)
	}
}

func TestStorageCheckFallsBackToBucketListingOnOlderAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	comparisonTestNode(t, f, "storage-legacy", &storageCheckAgent{s3: s3, legacy: true})
	url := f.http.URL + "/api/master/s3-config"
	virtual := false
	// The older Agent cannot check the bucket, so the save succeeds.
	code, body := request(t, f.http.Client(), "POST", url, f.admin, storageCheckConfig("http://storage.example.com", &virtual))
	if code != 200 {
		t.Fatalf("legacy save: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets", f.admin, nil)
	if code != 200 || body["bucketCheckSupported"] != false || fmt.Sprint(body["buckets"]) != "[src]" {
		t.Fatalf("legacy test: HTTP %d %v", code, body)
	}
	s3.set("SignatureDoesNotMatch", "")
	code, _, body, err := localizedRequest(f.http.Client(), "GET", url+"/selfhosted/buckets", f.admin, "en-US", nil)
	if err != nil || code != 502 || body["message"] != "Signature mismatch. Check the Secret Key and the signature version." || body["reasonCode"] != "STORAGE_SIGNATURE_MISMATCH" {
		t.Fatalf("legacy failure: HTTP %d %v %v", code, body, err)
	}
}

func TestStorageEndpointValidationIsLocalized(t *testing.T) {
	f := newAgentHTTPFixture(t)
	config := storageCheckConfig("host.docker.internal:18333", nil)
	code, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, "zh-CN", config)
	if err != nil || code != 400 || !strings.HasPrefix(fmt.Sprint(body["message"]), "Endpoint 需以 http:// 或 https:// 开头") {
		t.Fatalf("endpoint without scheme: HTTP %d %v %v", code, body, err)
	}
}

// A failed check of the first listed bucket, which nobody named, does not
// block: that bucket can be in another region while the others work. The
// buckets are listed and the failure is a warning. A bucket the user names
// is still checked strictly, and an addressing failure, which holds for every
// bucket, still blocks saving.
func TestStorageCheckWarnsWhenTheFirstListedBucketFails(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	comparisonTestNode(t, f, "storage-first-bucket", &storageCheckAgent{s3: s3})
	url := f.http.URL + "/api/master/s3-config"
	s3.set("", "PermanentRedirect")
	code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", storageCheckConfig("http://storage.example.com:9000", nil))
	if err != nil || code != 200 || !strings.HasPrefix(fmt.Sprint(body["warning"]), "连接可用，但对第一个列出的存储桶 src 的检查失败：") ||
		!strings.Contains(fmt.Sprint(body["warningDetail"]), "PermanentRedirect") || strings.Contains(fmt.Sprint(body["warningDetail"]), "secret-fixture-key") {
		t.Fatalf("save: HTTP %d %v %v", code, body, err)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets", f.admin, nil)
	if code != 200 || fmt.Sprint(body["buckets"]) != "[src]" || body["checkedBucket"] != nil || body["bucketCheckFailed"] != "src" || body["warning"] == nil {
		t.Fatalf("bucket list: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=src", f.admin, nil)
	if code != 502 || body["reasonCode"] != "STORAGE_ERROR" {
		t.Fatalf("named bucket: HTTP %d %v", code, body)
	}

	// Virtual-hosted on a self-hosted endpoint: saving is refused as before,
	// but the bucket picker of a saved connection still gets the list.
	s3.set("", "")
	virtual := false
	config := storageCheckConfig("http://storage.example.com", &virtual)
	config["alias"] = "virtual"
	if code, body := request(t, f.http.Client(), "POST", url, f.admin, config); code != 400 || body["pathStyleHint"] != true {
		t.Fatalf("addressing save: HTTP %d %v", code, body)
	}
	config["validate"] = false
	if code, body := request(t, f.http.Client(), "POST", url, f.admin, config); code != 200 {
		t.Fatalf("unvalidated save: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/virtual/buckets", f.admin, nil)
	if code != 200 || fmt.Sprint(body["buckets"]) != "[src]" || body["bucketCheckFailed"] != "src" || body["warningPathStyleHint"] != true ||
		!strings.Contains(fmt.Sprint(body["warning"]), "Path-style") {
		t.Fatalf("addressing bucket list: HTTP %d %v", code, body)
	}
}

// The permission check reports each operation, explains failures in the
// caller's language without credentials, and names an Agent too old for it.
func TestStoragePermissionCheckReportsEachOperation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	agent := &storageCheckAgent{s3: s3}
	comparisonTestNode(t, f, "storage-permissions", agent)
	config := storageCheckConfig("http://storage.example.com:9000", nil)
	config["validate"] = false
	if code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, config); code != 200 {
		t.Fatalf("save: HTTP %d %v", code, body)
	}
	url := f.http.URL + "/api/master/s3-config/selfhosted/permissions"
	code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", map[string]any{"bucket": "src", "write": true})
	if err != nil || code != 200 || body["bucket"] != "src" || body["write"] != true {
		t.Fatalf("check: HTTP %d %v %v", code, body, err)
	}
	results, _ := body["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results %v", body["results"])
	}
	put, _ := results[3].(map[string]any)
	if put["status"] != "FAILED" || put["reasonCode"] != "STORAGE_ACCESS_DENIED" || !strings.HasPrefix(fmt.Sprint(put["message"]), "访问被拒绝") ||
		strings.Contains(fmt.Sprint(put["detail"]), "secret-fixture-key") {
		t.Fatalf("PUT row %v", put)
	}
	if code, body := request(t, f.http.Client(), "POST", url, f.admin, map[string]any{"bucket": ""}); code != 400 {
		t.Fatalf("empty bucket: HTTP %d %v", code, body)
	}
	agent.legacy = true
	code, _, body, _ = localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", map[string]any{"bucket": "src"})
	if code != 409 || !strings.Contains(fmt.Sprint(body["message"]), "执行节点版本较旧") {
		t.Fatalf("older Agent: HTTP %d %v", code, body)
	}
}
