package transferstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Alibaba Cloud OSS lists the buckets of every region but answers a request
// for a bucket in another region with 403 AccessDenied, saying that the bucket
// must be addressed through the endpoint it names. The error keeps that
// endpoint, so the Master can name it instead of blaming the permissions.
func TestConnectionCheckKeepsTheEndpointABucketMustUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Server", "AliyunOSS")
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>a-logs</Name></Bucket><Bucket><Name>b-data</Name></Bucket></Buckets></ListAllMyBucketsResult>")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>AccessDenied</Code>
  <Message>The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.</Message>
  <RequestId>5F1A</RequestId>
  <HostId>a-logs.oss-cn-hangzhou.aliyuncs.com</HostId>
  <Bucket>a-logs</Bucket>
  <Endpoint>oss-cn-beijing.aliyuncs.com</Endpoint>
  <EC>0003-00001403</EC>
</Error>`)
	}))
	defer server.Close()
	check, err := CheckConnection(context.Background(), S3Config{Endpoint: server.URL, AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true, HTTPClient: server.Client()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if check.Bucket != "a-logs" || check.BucketErrorKind != StorageErrorAccessDenied {
		t.Fatalf("check: %+v", check)
	}
	mismatch, endpoint := EndpointMismatch(check.BucketError)
	if !mismatch || endpoint != "oss-cn-beijing.aliyuncs.com" {
		t.Fatalf("endpoint mismatch not recognized: %v %q in %s", mismatch, endpoint, check.BucketError)
	}
	for _, secret := range []string{"test-access", "test-secret", "5F1A</RequestId>"} {
		if strings.Contains(check.BucketError, secret) {
			t.Fatalf("error exposes %q: %s", secret, check.BucketError)
		}
	}
	// Text relayed by older Agents carries the message without the endpoint;
	// other refusals are not mistaken for it.
	if mismatch, endpoint := EndpointMismatch("api error AccessDenied: The bucket you are attempting to access must be addressed using the specified endpoint."); !mismatch || endpoint != "" {
		t.Fatalf("relayed text: %v %q", mismatch, endpoint)
	}
	if mismatch, _ := EndpointMismatch("api error AccessDenied: Access Denied [storage response: http=403, body=xml, endpoint=x.example]"); mismatch {
		t.Fatal("a plain AccessDenied was taken for an endpoint mismatch")
	}
}
