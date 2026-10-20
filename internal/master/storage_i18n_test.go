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
