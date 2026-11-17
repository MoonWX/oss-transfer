package master

import (
	"net/http"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

func TestStorageWithoutAgentsExplainsValidationInRequestLanguage(t *testing.T) {
	f := newAgentHTTPFixture(t)
	config := map[string]any{
		"alias": "storage", "endpoint": "http://127.0.0.1:9",
		"accessKey": "test-key", "secretKey": "test-secret", "validate": true,
	}
	for _, tc := range []struct{ locale, prefix, message string }{
		{"zh-CN", "存储验证失败：", "没有在线执行节点，请先添加执行节点再测试连接。"},
		{"en-US", "Storage validation failed: ", "No Agent is online. Add an execution node before testing this connection."},
		{"ja-JP", "ストレージの検証に失敗しました：", "オンラインの実行ノードがありません。実行ノードを追加してから接続をテストしてください。"},
		{"", "Storage validation failed: ", "no Agent is online; join an Agent before validating storage"},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			code, _, response, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, tc.locale, config)
			if err != nil || code != http.StatusBadRequest || response["message"] != tc.prefix+tc.message {
				t.Fatalf("save: HTTP %d, body=%v, error=%v", code, response, err)
			}
			snapshot, err := storageconfig.NewMaster(f.db).Snapshot()
			if err != nil || len(snapshot.Connections) != 0 {
				t.Fatalf("validation failure saved credentials: %v, %v", snapshot, err)
			}
			config["validate"] = false
			code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, config)
			if code != http.StatusOK {
				t.Fatalf("save without validation: HTTP %d", code)
			}
			code, _, response, err = localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/master/s3-config/storage/buckets", f.admin, tc.locale, nil)
			if err != nil || code != http.StatusBadGateway || response["message"] != tc.message {
				t.Fatalf("list: HTTP %d, body=%v, error=%v", code, response, err)
			}
			if _, err := storageconfig.NewMaster(f.db).Delete("storage"); err != nil {
				t.Fatal(err)
			}
			config["validate"] = true
		})
	}
}

// Agents are registered but none is online: adding another node is not the
// fix, so the check says that the registered nodes are offline.
func TestStorageCheckWithEveryRegisteredAgentOfflineSaysSo(t *testing.T) {
	f := newAgentHTTPFixture(t)
	offlineComparisonNode(t, f, "offline-agent")
	config := map[string]any{"alias": "storage", "endpoint": "http://127.0.0.1:9", "accessKey": "test-key", "secretKey": "test-secret", "validate": false}
	if code, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, config); code != http.StatusOK {
		t.Fatalf("save without validation: HTTP %d", code)
	}
	want := "已注册的执行节点均不在线，请先恢复其中一个节点的服务或网络，再测试连接。"
	code, _, response, err := localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/master/s3-config/storage/buckets", f.admin, "zh-CN", nil)
	if err != nil || code != http.StatusBadGateway || response["message"] != want {
		t.Fatalf("list: HTTP %d, body=%v, error=%v", code, response, err)
	}
	code, _, response, err = localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/s3-config/storage/permissions", f.admin, "zh-CN", map[string]any{"bucket": "photos"})
	if err != nil || code != http.StatusConflict || response["message"] != want {
		t.Fatalf("permissions: HTTP %d, body=%v, error=%v", code, response, err)
	}
	code, _, response, err = localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/master/s3-config/storage/buckets", f.admin, "en-US", nil)
	if err != nil || response["message"] != "Every registered execution node is offline. Bring one back online before testing this connection." {
		t.Fatalf("list (en): HTTP %d, body=%v, error=%v", code, response, err)
	}
}
