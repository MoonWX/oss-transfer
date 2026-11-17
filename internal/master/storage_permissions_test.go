package master

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// prefixPermissionAgent answers the permission check like an Agent whose key
// may write only below backup/ and may not abort multipart uploads. A legacy
// one predates the write prefix and refuses the unknown field, as Agents
// decode commands strictly.
type prefixPermissionAgent struct {
	mu       sync.Mutex
	legacy   bool
	requests []agentservice.StoragePermissionsRequest
}

func (a *prefixPermissionAgent) ReplaySafe(string) bool { return true }

func (a *prefixPermissionAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := json.Unmarshal(c.Payload, &snapshot); err != nil {
			return agenttransport.Response{}, err
		}
		return fakeReply(200, map[string]any{"revision": snapshot.Revision})
	case "storage_permissions":
		a.mu.Lock()
		legacy := a.legacy
		a.mu.Unlock()
		var request agentservice.StoragePermissionsRequest
		var err error
		if legacy {
			var older struct {
				Config storageconfig.Config `json:"config"`
				Bucket string               `json:"bucket"`
				Write  bool                 `json:"write,omitempty"`
			}
			err = fakeDecode(c.Payload, &older)
			request = agentservice.StoragePermissionsRequest{Config: older.Config, Bucket: older.Bucket, Write: older.Write}
		} else {
			err = fakeDecode(c.Payload, &request)
		}
		if err != nil {
			return fakeReply(400, map[string]string{"code": "invalid_request", "message": err.Error()})
		}
		a.mu.Lock()
		a.requests = append(a.requests, request)
		a.mu.Unlock()
		probe := request.Prefix + transferstore.PermissionProbePrefix + "x"
		check := transferstore.PermissionCheck{Bucket: request.Bucket, PathStyle: true, Write: request.Write, Prefix: request.Prefix, Results: []transferstore.PermissionResult{
			{Operation: transferstore.PermissionList, Status: transferstore.PermissionPassed},
			{Operation: transferstore.PermissionHead, Status: transferstore.PermissionPassed, Key: "folder/"},
			{Operation: transferstore.PermissionGet, Status: transferstore.PermissionPassed, Key: "folder/"},
			{Operation: transferstore.PermissionPut, Status: transferstore.PermissionPassed, Key: probe},
			{Operation: transferstore.PermissionHeadWritten, Status: transferstore.PermissionPassed, Key: probe},
			{Operation: transferstore.PermissionMultipart, Status: transferstore.PermissionPassed, Key: probe + "-multipart"},
			{Operation: transferstore.PermissionMultipartAbort, Status: transferstore.PermissionFailed, Key: probe + "-multipart", Error: "api error AccessDenied: denied for secret-fixture-key", ErrorKind: transferstore.StorageErrorAccessDenied},
			{Operation: transferstore.PermissionDeleteProbe, Status: transferstore.PermissionPassed, Key: probe},
		}, LeftoverUploadKey: probe + "-multipart", LeftoverUploadID: "upload-1"}
		return fakeReply(200, check)
	}
	return fakeUnknownOperation()
}

// The check writes below a prefix for keys limited to one, reports a denied
// abort as its own row with the upload it left, and an Agent too old for a
// write prefix is named before it writes anything elsewhere.
func TestStoragePermissionCheckWritesBelowAPrefix(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := &prefixPermissionAgent{}
	comparisonTestNode(t, f, "storage-prefix", agent)
	config := storageCheckConfig("http://storage.example.com:9000", nil)
	config["validate"] = false
	if code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, config); code != 200 {
		t.Fatalf("save: HTTP %d %v", code, body)
	}
	url := f.http.URL + "/api/master/s3-config/selfhosted/permissions"
	code, _, body, err := localizedRequest(f.http.Client(), "POST", url, f.admin, "zh-CN", map[string]any{"bucket": "dst", "write": true, "prefix": "backup/"})
	if err != nil || code != 200 || body["prefix"] != "backup/" || body["leftoverUploadKey"] != "backup/"+transferstore.PermissionProbePrefix+"x-multipart" || body["leftoverUploadId"] != "upload-1" {
		t.Fatalf("check: HTTP %d %v %v", code, body, err)
	}
	results, _ := body["results"].([]any)
	abort, _ := results[6].(map[string]any)
	if len(results) != 8 || abort["operation"] != "MULTIPART_ABORT" || abort["status"] != "FAILED" || !strings.HasPrefix(fmt.Sprint(abort["message"]), "访问被拒绝") || strings.Contains(fmt.Sprint(abort["detail"]), "secret-fixture-key") {
		t.Fatalf("results %v", body["results"])
	}
	// Without a write check there is nothing to write below a prefix.
	if code, body := request(t, f.http.Client(), "POST", url, f.admin, map[string]any{"bucket": "dst", "prefix": "backup/"}); code != 200 || body["prefix"] != nil {
		t.Fatalf("read-only check: HTTP %d %v", code, body)
	}
	agent.mu.Lock()
	requests := append([]agentservice.StoragePermissionsRequest(nil), agent.requests...)
	agent.mu.Unlock()
	if len(requests) != 2 || requests[0].Prefix != "backup/" || !requests[0].Write || requests[1].Prefix != "" || requests[1].Write {
		t.Fatalf("Agent requests %+v", requests)
	}
	for locale, want := range map[string]string{
		"zh-CN": "写入位置前缀不能超过 512 字节，且不能包含控制字符",
		"en-US": "The write prefix must be at most 512 bytes of text without control characters",
		"ja-JP": "書き込み先のプレフィックスは 512 バイト以内で、制御文字を含めないでください",
	} {
		code, _, body, _ := localizedRequest(f.http.Client(), "POST", url, f.admin, locale, map[string]any{"bucket": "dst", "write": true, "prefix": "bad\nprefix"})
		if code != 400 || body["message"] != want {
			t.Fatalf("%s: invalid prefix: HTTP %d %v", locale, code, body)
		}
	}

	agent.mu.Lock()
	agent.legacy = true
	agent.mu.Unlock()
	for locale, want := range map[string]string{
		"zh-CN": "执行节点版本较旧，不支持在指定前缀下检查写入；请先更新执行节点，或不填写写入位置前缀再检查",
		"en-US": "The execution node is too old to check writing below a prefix; update it first, or check without a write prefix",
		"ja-JP": "実行ノードのバージョンが古いため、プレフィックスを指定した書き込みは確認できません。先に実行ノードを更新するか、書き込み先のプレフィックスを指定せずに確認してください",
	} {
		code, _, body, _ := localizedRequest(f.http.Client(), "POST", url, f.admin, locale, map[string]any{"bucket": "dst", "write": true, "prefix": "backup/"})
		if code != 409 || body["message"] != want {
			t.Fatalf("%s: older Agent with a prefix: HTTP %d %v", locale, code, body)
		}
	}
	if code, body := request(t, f.http.Client(), "POST", url, f.admin, map[string]any{"bucket": "dst", "write": true}); code != 200 || body["prefix"] != nil {
		t.Fatalf("older Agent without a prefix: HTTP %d %v", code, body)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.requests) != 3 || agent.requests[2].Prefix != "" {
		t.Fatalf("the older Agent ran a check it could not place: %+v", agent.requests)
	}
}
