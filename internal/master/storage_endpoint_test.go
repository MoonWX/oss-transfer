package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// ossRegionAgent runs the real connection and permission checks against an
// OSS-like server: ListBuckets lists the buckets of every region, and a bucket
// of another region answers 403 AccessDenied naming the endpoint to use.
type ossRegionAgent struct{ s3 *storageCheckS3 }

func (a *ossRegionAgent) ReplaySafe(string) bool { return true }

func (a *ossRegionAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	var request struct {
		Config storageconfig.Config `json:"config"`
		Bucket string               `json:"bucket"`
		Write  bool                 `json:"write"`
		Prefix string               `json:"prefix"`
	}
	switch c.Operation {
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := json.Unmarshal(c.Payload, &snapshot); err != nil {
			return agenttransport.Response{}, err
		}
		return fakeReply(200, map[string]any{"revision": snapshot.Revision})
	case "storage_check", "storage_permissions":
		if err := fakeDecode(c.Payload, &request); err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		bucket := request.Bucket
		if bucket == "" {
			bucket = "validation"
		}
		settings, err := request.Config.S3Config(bucket)
		if err != nil {
			return fakeReply(400, map[string]string{"message": err.Error()})
		}
		settings.HTTPClient = a.s3.client()
		if c.Operation == "storage_check" {
			check, err := transferstore.CheckConnection(ctx, settings, request.Bucket)
			if err != nil {
				return fakeReply(500, map[string]string{"message": err.Error()})
			}
			return fakeReply(200, check)
		}
		check, err := transferstore.CheckBucketPermissions(ctx, settings, request.Write, request.Prefix)
		if err != nil {
			return fakeReply(500, map[string]string{"message": err.Error()})
		}
		return fakeReply(200, check)
	}
	return fakeUnknownOperation()
}

func newOSSRegionS3(t *testing.T, denied string) *storageCheckS3 {
	s := &storageCheckS3{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Server", "AliyunOSS")
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>a-logs</Name></Bucket><Bucket><Name>b-data</Name></Bucket></Buckets></ListAllMyBucketsResult>")
		case strings.HasPrefix(r.URL.Path, "/a-logs") && denied == "region":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.</Message><RequestId>5F1A</RequestId><HostId>a-logs.oss-cn-hangzhou.aliyuncs.com</HostId><Bucket>a-logs</Bucket><Endpoint>oss-cn-beijing.aliyuncs.com</Endpoint><EC>0003-00001403</EC></Error>`)
		case strings.HasPrefix(r.URL.Path, "/a-logs") && denied == "policy":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>You have no right to access this object because of bucket acl.</Message></Error>`)
		default:
			fmt.Fprint(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

// A bucket of another region is not a permission problem: the connection test,
// the save-time check and the permission check name the endpoint the bucket
// must use, and the failure summary of a task on it says so too.
func TestStorageChecksNameTheEndpointOfABucketInAnotherRegion(t *testing.T) {
	f := newAgentHTTPFixture(t)
	comparisonTestNode(t, f, "oss-region", &ossRegionAgent{s3: newOSSRegionS3(t, "region")})
	url := f.http.URL + "/api/master/s3-config"
	endpoint := "存储桶 a-logs 需要通过 Endpoint oss-cn-beijing.aliyuncs.com 访问，这通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该 Endpoint 创建存储连接。"
	// Saving checks the first listed bucket, which nobody named.
	code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", storageCheckConfig("http://storage.example.com:9000", nil))
	if err != nil || code != 200 || body["warning"] != "连接可用，但对第一个列出的存储桶 a-logs 的检查失败："+endpoint || body["warningCode"] != "STORAGE_WRONG_ENDPOINT" ||
		!strings.Contains(fmt.Sprint(body["warningDetail"]), "endpoint=oss-cn-beijing.aliyuncs.com") || strings.Contains(fmt.Sprint(body["warning"]), "权限") {
		t.Fatalf("save: HTTP %d %v %v", code, body, err)
	}
	// Testing the bucket by name still lists the buckets and warns.
	code, _, body, err = localizedRequest(f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=a-logs", f.admin, "zh-CN", nil)
	if err != nil || code != 200 || body["warning"] != endpoint || body["bucketCheckFailed"] != "a-logs" {
		t.Fatalf("named bucket: HTTP %d %v %v", code, body, err)
	}
	code, _, body, err = localizedRequest(f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=a-logs", f.admin, "en-US", nil)
	if err != nil || code != 200 || body["warning"] != "Bucket a-logs must be accessed through endpoint oss-cn-beijing.aliyuncs.com, usually the endpoint of the region the bucket is in. To migrate this bucket, create a storage connection with that endpoint." {
		t.Fatalf("named bucket (en): HTTP %d %v %v", code, body, err)
	}
	code, _, body, err = localizedRequest(f.http.Client(), "POST", url+"/selfhosted/permissions", f.admin, "zh-CN", map[string]any{"bucket": "a-logs"})
	results, _ := body["results"].([]any)
	if err != nil || code != 200 || len(results) == 0 {
		t.Fatalf("permissions: HTTP %d %v %v", code, body, err)
	}
	if list := results[0].(map[string]any); list["status"] != transferstore.PermissionFailed || list["message"] != endpoint || list["reasonCode"] != "STORAGE_WRONG_ENDPOINT" {
		t.Fatalf("permission row: %v", list)
	}
	// The failure summary of a task on such a bucket names the cause.
	exception := task.Exception{Error: `list objects: operation error S3: ListObjectsV2, https response error StatusCode: 403, RequestID: 5F1A, HostID: a-logs.oss-cn-hangzhou.aliyuncs.com, api error AccessDenied: The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.`}
	if code, message, _ := failureCause(exception); code != "WRONG_ENDPOINT" || !strings.Contains(message, "Endpoint") {
		t.Fatalf("failure cause: %s %s", code, message)
	}
	// Each object first HEADs the destination. OSS answers that HEAD with
	// 403 and no body; only its EC header, kept in the storage response
	// diagnostic, says why.
	head := task.Exception{Error: `check destination "a.bin": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: 5F1A, HostID: , api error Forbidden: Forbidden [storage response: http=403, server=AliyunOSS, request-id=5F1A, ec=0003-00001403, protocol=HTTP/1.1, connection=new, request-length=0, body=empty]`}
	if code, message, _ := failureCause(head); code != "WRONG_ENDPOINT" || !strings.Contains(message, "Endpoint") {
		t.Fatalf("HEAD failure cause: %s %s", code, message)
	}
	head.Error = strings.Replace(head.Error, "ec=0003-00001403", "ec=0003-00000201", 1)
	if code, _, _ := failureCause(head); code != "ACCESS_DENIED" {
		t.Fatalf("another refused HEAD: %s", code)
	}
}

// The first listed bucket was not chosen by anyone: whatever it answers, the
// warning says that the connection works and only that bucket's check failed.
func TestStorageCheckOfTheFirstListedBucketStaysNeutral(t *testing.T) {
	f := newAgentHTTPFixture(t)
	comparisonTestNode(t, f, "oss-policy", &ossRegionAgent{s3: newOSSRegionS3(t, "policy")})
	code, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, "zh-CN", storageCheckConfig("http://storage.example.com:9000", nil))
	if err != nil || code != 200 || body["warning"] != "连接可用，但对第一个列出的存储桶 a-logs 的检查失败：访问被拒绝：凭据有效，但没有执行此请求的权限。" || body["warningCode"] != "STORAGE_ACCESS_DENIED" {
		t.Fatalf("save: HTTP %d %v %v", code, body, err)
	}
	// A bucket the user names keeps the definite permission sentence.
	code, _, body, err = localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/master/s3-config/selfhosted/buckets?bucket=a-logs", f.admin, "zh-CN", nil)
	if err != nil || code != 200 || body["warning"] != "该凭据无权列举存储桶 a-logs 中的对象（AccessDenied）。从此存储桶迁移前，请检查存储桶权限。" {
		t.Fatalf("named bucket: HTTP %d %v %v", code, body, err)
	}
}
