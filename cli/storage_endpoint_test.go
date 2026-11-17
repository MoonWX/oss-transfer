package main

import (
	"io"
	"strings"
	"testing"
)

// A bucket in another region is refused by Alibaba Cloud OSS with
// AccessDenied, but the reply says which endpoint the bucket needs: the
// failure reason and the exception list say that, not that the credentials
// lack permission.
func TestStorageErrorsNameTheEndpointABucketNeeds(t *testing.T) {
	const relayed = `list objects: operation error S3: ListObjectsV2, https response error StatusCode: 403, RequestID: 5F1A, HostID: a-logs.oss-cn-hangzhou.aliyuncs.com, api error AccessDenied: The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.`
	const named = relayed + ` [storage response: http=403, server=AliyunOSS, body=xml, endpoint=oss-cn-beijing.aliyuncs.com]`
	for _, test := range []struct {
		locale cliLocale
		text   string
		want   string
	}{
		{localeZH, named, "存储桶需要通过 Endpoint oss-cn-beijing.aliyuncs.com 访问，这通常是该存储桶所在地域的 Endpoint。"},
		{localeEN, named, "The bucket must be accessed through endpoint oss-cn-beijing.aliyuncs.com, usually the endpoint of the region the bucket is in."},
		{localeJA, named, "このバケットにはエンドポイント oss-cn-beijing.aliyuncs.com でアクセスする必要があります。"},
		{localeZH, relayed, "存储桶需要通过另一个 Endpoint 访问，通常是该存储桶所在地域的 Endpoint。"},
	} {
		out := &cliLocaleWriter{Writer: io.Discard, locale: test.locale}
		if got := storageErrorSummary(out, test.text); !strings.HasPrefix(got, test.want) {
			t.Errorf("%s summary: %q, want prefix %q", test.locale, got, test.want)
		}
		if got := cliErrorSummary(out, test.text); strings.Contains(got, "权限") || strings.Contains(got, "permission") {
			t.Errorf("%s exception reason blames permissions: %q", test.locale, got)
		}
	}
	zh := &cliLocaleWriter{Writer: io.Discard, locale: localeZH}
	if got := cliErrorSummary(zh, named); got != "存储桶需要通过其所在地域的 Endpoint 访问，请用该 Endpoint 添加存储连接" {
		t.Fatalf("exception reason: %q", got)
	}
	// A refusal that is about permissions keeps the permission advice.
	if got := storageErrorSummary(zh, "api error AccessDenied: Access Denied"); got != "访问被拒绝：凭据没有执行此操作的权限，请检查存储桶权限。" {
		t.Fatalf("plain AccessDenied: %q", got)
	}
}
