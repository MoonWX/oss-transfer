package main

import (
	"net/http"
	"strings"
	"testing"
)

// One node runs the connection test and the validation on save. The CLI names
// it in passes and failures, as the Master reports it.
func TestStorageChecksNameTheNodeThatRanThem(t *testing.T) {
	t.Setenv("CLI_TEST_ACCESS", "access")
	t.Setenv("CLI_TEST_SECRET", "secret")
	const node = "Office LAN (10.0.0.8)"
	fail := false
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/master/s3-config/lan/buckets" && fail:
			w.WriteHeader(http.StatusBadGateway)
			adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "连接 10.0.0.5:9000 超时，请检查执行节点到 Endpoint 的网络和防火墙。", "reasonCode": "STORAGE_TIMEOUT", "node": node})
		case r.URL.Path == "/api/master/s3-config/lan/buckets":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "buckets": []interface{}{"data"}, "pathStyle": true, "pathStyleAuto": true, "checkedBucket": "data", "node": node})
		case r.URL.Path == "/api/master/s3-config" && fail:
			w.WriteHeader(http.StatusBadRequest)
			adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "存储验证失败：连接 10.0.0.5:9000 超时，请检查执行节点到 Endpoint 的网络和防火墙。", "reasonCode": "STORAGE_TIMEOUT", "node": node})
		case r.URL.Path == "/api/master/s3-config":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "message": "存储连接已保存", "successCount": 1, "failedCount": 0, "partial": false, "details": []interface{}{node + ": 同步成功"}, "validationNode": node})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}
	add := []string{"storage", "add", "lan", "--endpoint", "http://10.0.0.5:9000", "--access-key-env", "CLI_TEST_ACCESS", "--secret-key-env", "CLI_TEST_SECRET"}

	code, out, stderr := runMockCLI(t, []string{"storage", "test", "lan"}, handler)
	if code != 0 || !strings.Contains(out, "执行检查的节点: "+node) || !strings.Contains(out, "连接测试通过") {
		t.Fatalf("passed test: %d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, add, handler)
	if code != 0 || !strings.Contains(out, "保存前已由执行节点 "+node+" 验证连接") {
		t.Fatalf("validated save: %d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, []string{"--lang", "en", "storage", "test", "lan"}, handler)
	if code != 0 || !strings.Contains(out, "Checked on node: "+node) {
		t.Fatalf("English: %d %s %s", code, out, stderr)
	}

	// A failure is one error line that names the node, and nothing else.
	fail = true
	code, out, stderr = runMockCLI(t, []string{"storage", "test", "lan"}, handler)
	if code != 1 || out != "" || !strings.Contains(stderr, "连接 10.0.0.5:9000 超时") || !strings.Contains(stderr, "执行检查的节点: "+node) {
		t.Fatalf("failed test: %d %q %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, add, handler)
	if code != 1 || out != "" || !strings.Contains(stderr, "存储验证失败") || !strings.Contains(stderr, "执行检查的节点: "+node) {
		t.Fatalf("failed validation: %d %q %s", code, out, stderr)
	}
	code, out, _ = runMockCLI(t, append(add, "--json"), handler)
	if code != 1 || !strings.Contains(out, `"node": "`+node+`"`) {
		t.Fatalf("JSON keeps the node: %d %s", code, out)
	}
	code, _, stderr = runMockCLI(t, append([]string{"--lang", "ja"}, add...), handler)
	if code != 1 || !strings.Contains(stderr, "確認したノード: "+node) {
		t.Fatalf("Japanese: %d %s", code, stderr)
	}
}
