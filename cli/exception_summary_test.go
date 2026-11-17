package main

import (
	"fmt"
	"io"
	"testing"
)

// exceptions list names the cause of each failed object as the Master's task
// summary does: by the storage reply's error code, by its status only for a
// reply without one, and never from words of the object key. Most keys below
// contain "eof", "timeout" or "403".
func TestExceptionSummariesClassifyRepliesByCodeNotByKey(t *testing.T) {
	const reply = "https response error StatusCode: %d, RequestID: REQ1, HostID: H1, %s [storage response: http=%d, body=xml]"
	get := func(key string, status int, code string) string {
		return fmt.Sprintf("open source %q: operation error S3: GetObject, ", key) + fmt.Sprintf(reply, status, code, status)
	}
	put := func(key string, status int, code string) string {
		return fmt.Sprintf("write destination %q: operation error S3: PutObject, exceeded maximum number of attempts, 10, ", key) + fmt.Sprintf(reply, status, code, status)
	}
	sent := func(key string, attempts int) string {
		return fmt.Sprintf("write destination %q: operation error S3: PutObject, exceeded maximum number of attempts, %d, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put \"http://storage.example:9000/bucket/%s?x-id=PutObject\": EOF", key, attempts, key)
	}
	for _, test := range []struct{ message, want string }{
		{get("videofiles/Geoffrey.mp4", 403, "InvalidObjectState: The operation is not valid for the object's storage class"), "源对象为归档存储，需先解冻再重试"},
		{get("videofiles/Geoffrey.mp4", 403, "api error AccessDenied: Access Denied"), "存储服务拒绝访问，请检查凭据对该存储桶的权限"},
		{`check destination "k": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden`, "存储服务拒绝访问，请检查凭据对该存储桶的权限"},
		// OSS refuses a HEAD of a bucket in another region with 403 and no
		// body; only its EC, kept in the diagnostic, names the cause.
		{`check destination "k": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden [storage response: http=403, server=AliyunOSS, request-id=5F1A, ec=0003-00001403, body=empty]`, "存储桶需要通过其所在地域的 Endpoint 访问，请用该 Endpoint 添加存储连接"},
		{`check destination "k": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden [storage response: http=403, server=AliyunOSS, request-id=5F1A, ec=0003-00000201, body=empty]`, "存储服务拒绝访问，请检查凭据对该存储桶的权限"},
		{put("photos/403.jpg", 503, "api error SlowDown: Please reduce your request rate."), "存储服务限流，可稍后重试失败对象"},
		{put("photos/403.jpg", 429, "api error TooManyRequests: Too many requests"), "存储服务限流，可稍后重试失败对象"},
		{put("photos/403.jpg", 503, "api error ServiceUnavailable: Service Unavailable"), "存储服务端出错，可在存储恢复后重试失败对象"},
		{put("videofiles/Geoffrey.mp4", 500, "api error InternalError: We encountered an internal error."), "存储服务端出错，可在存储恢复后重试失败对象"},
		{get("logs/timeout.log", 403, "api error AccessDenied: Access Denied"), "存储服务拒绝访问，请检查凭据对该存储桶的权限"},
		// A storage that stopped answering a running task, after the ten
		// attempts of a host that answered before, or a host that never did.
		{sent("logs/timeout/a.log", 10), "连接意外中断，可重试失败对象"},
		{sent("k", 3), "连接在存储服务应答前关闭，请检查 Endpoint 地址、端口和协议"},
		{`write destination "k": operation error S3: UploadPart, exceeded maximum number of attempts, 10, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put "https://h/b/k": write tcp 10.0.0.2:51234->10.0.0.9:443: write: connection reset by peer`, "连接被存储服务中断，可重试失败对象"},
	} {
		if got := cliErrorSummary(io.Discard, test.message); got != test.want {
			t.Errorf("%s\n  got  %q\n  want %q", test.message, got, test.want)
		}
	}
	// Unknown refusals and failures keep their own first line.
	skewed := get("videofiles/Geoffrey.mp4", 403, "api error RequestTimeTooSkewed: The difference between the request time and the current time is too large.")
	if got := cliErrorSummary(io.Discard, skewed); got != cliTruncate(skewed, 60) {
		t.Errorf("unknown refusal: %q", got)
	}
	verify := `verify write "users/Geoffrey/EOF.txt": destination did not confirm the complete object`
	if got := cliErrorSummary(io.Discard, verify); got != cliTruncate(verify, 60) {
		t.Errorf("a key was taken for the cause: %q", got)
	}
	keys := []string{"源对象为归档存储，需先解冻再重试", "存储服务限流，可稍后重试失败对象", "存储服务端出错，可在存储恢复后重试失败对象", "连接在存储服务应答前关闭，请检查 Endpoint 地址、端口和协议"}
	for _, key := range cliStorageCodeTexts {
		keys = append(keys, key)
	}
	for _, key := range keys {
		if values, ok := cliMessages[key]; !ok || values[0] == "" || values[1] == "" {
			t.Errorf("%q lacks a translation: %v", key, values)
		}
	}
}
