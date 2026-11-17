package transferstore

import "testing"

// The SDK reports a code it does not model after "api error", one it models
// (InvalidObjectState for GetObject) bare after the request IDs, and a reply
// without a body under its status text, which carries no code of its own.
func TestStorageErrorReplyReadsTheCodeAndStatus(t *testing.T) {
	for _, test := range []struct {
		text   string
		code   string
		status int
	}{
		{`operation error S3: GetObject, https response error StatusCode: 403, RequestID: R, HostID: H, InvalidObjectState: The operation is not valid for the object's storage class [storage response: http=403]`, "InvalidObjectState", 403},
		{`operation error S3: PutObject, exceeded maximum number of attempts, 10, https response error StatusCode: 503, RequestID: R, HostID: H, api error SlowDown: Please reduce your request rate.`, "SlowDown", 503},
		{`operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden`, "", 403},
		{`operation error S3: PutObject, https response error StatusCode: 429, RequestID: , HostID: , api error TooManyRequests: Too Many Requests`, "", 429},
		{`<Error><Code>AccessDenied</Code></Error>`, "AccessDenied", 0},
		{`operation error S3: PutObject, exceeded maximum number of attempts, 10, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put "http://h/b/k": EOF`, "", 0},
	} {
		if code, status := StorageErrorReply(test.text); code != test.code || status != test.status {
			t.Errorf("%s: %q %d, want %q %d", test.text, code, status, test.code, test.status)
		}
	}
	if !IsThrottleCode("SlowDown") || IsThrottleCode("ServiceUnavailable") {
		t.Error("throttling codes changed")
	}
}
