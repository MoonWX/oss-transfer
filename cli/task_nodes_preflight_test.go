package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A dry run that the Master refuses before checking any directory (an
// offline, outdated or updating Agent, a stale view) is a failed node change
// preflight; one that answers valid:false failed the shared directory
// preflight; one without an answer did not complete. Nothing is submitted in
// any case.
func TestTaskNodesTellsNodeChangeFromSharedDirectoryFailures(t *testing.T) {
	args := []string{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share", "--yes"}
	refused := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	refused.respond = func(map[string]interface{}) (int, interface{}) {
		return 409, map[string]interface{}{"success": false, "code": "AGENT_TOO_OLD", "message": "执行节点 delta (10.0.0.3) 版本过旧，无法加入运行中的任务，请先升级"}
	}
	code, _, stderr := runMockCLI(t, args, refused.handler(t))
	if code != 1 || len(refused.posts) != 1 || !strings.Contains(stderr, "节点调整预检未通过，尚未提交变更") || !strings.Contains(stderr, "版本过旧") || strings.Contains(stderr, "共享目录预检") {
		t.Fatalf("refused dry run: %d %v %s", code, refused.posts, stderr)
	}

	failed := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	failed.respond = func(map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": false, "dryRun": true, "valid": false, "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share", "valid": false, "pathType": "MISSING", "message": "源路径不存在"}}, "validationErrors": []interface{}{"delta (10.0.0.3): 源路径不存在"}, "view": failed.view}}
	}
	code, out, stderr := runMockCLI(t, args, failed.handler(t))
	if code != 1 || len(failed.posts) != 1 || !strings.Contains(stderr, "共享目录预检未通过，尚未提交变更") || strings.Contains(stderr, "节点调整预检") || !strings.Contains(out, "源路径不存在") {
		t.Fatalf("failed shared directory preflight: %d %v\n%s\n%s", code, failed.posts, out, stderr)
	}
	english := func(master *taskNodesFakeMaster) string {
		_, _, stderr := runMockCLI(t, append([]string{"--lang", "en"}, args...), master.handler(t))
		return stderr
	}
	if text := english(refused); !strings.Contains(text, "The node change preflight did not pass; no change was submitted") {
		t.Fatal("English refusal:", text)
	}
	if text := english(failed); !strings.Contains(text, "The shared directory preflight did not pass; no change was submitted") {
		t.Fatal("English shared directory failure:", text)
	}

	// No answer at all: nothing was checked.
	view := taskNodesTestView(true)
	posts := 0
	client := &ApiClient{baseUrl: "http://admin-cli-test.invalid/api/master", client: &http.Client{Transport: adminTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method == "POST" {
			posts++
			return nil, io.ErrUnexpectedEOF
		}
		body, _ := json.Marshal(map[string]interface{}{"success": true, "data": view})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	_, err := runTaskCommand(client, args[1:])
	if err == nil || posts != 1 || !strings.Contains(err.Error(), "节点调整预检未完成，尚未提交变更") {
		t.Fatal("unanswered dry run:", posts, err)
	}
}

// Each node's preflight note is shown, also when it passed, with what the
// write check of a download destination found.
func TestTaskNodesShowsEachNodesPreflightNote(t *testing.T) {
	check := map[string]interface{}{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share", "displayName": "delta", "displayAddress": "10.0.0.3", "valid": true, "pathType": "DIRECTORY", "code": "SHARED_STORAGE_VERIFIED", "message": "已写入测试文件，所有节点读取校验通过并已清理", "writeProbeDirectory": "/mnt/share", "effectiveUid": float64(1001), "freeBytes": float64(1 << 30)}
	for locale, want := range map[cliLocale]string{
		localeZH: "已写入测试文件，所有节点读取校验通过并已清理；写入检查目录 /mnt/share；Agent 运行 UID 1001；可用空间 1 GiB",
		localeEN: "已写入测试文件，所有节点读取校验通过并已清理; write check directory /mnt/share; Agent UID 1001; 1 GiB free",
	} {
		var buffer bytes.Buffer
		renderTaskNodeBindingChecks(&cliLocaleWriter{Writer: &buffer, locale: locale}, []map[string]interface{}{check})
		if text := buffer.String(); !strings.Contains(text, want) {
			t.Fatalf("%s note:\n%s", locale, text)
		}
	}
	// An upload, or an older Master, reports no write check.
	var buffer bytes.Buffer
	renderTaskNodeBindingChecks(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, []map[string]interface{}{{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share", "valid": true, "pathType": "DIRECTORY"}})
	if text := buffer.String(); strings.Contains(text, "写入检查目录") || !strings.Contains(text, "—") {
		t.Fatal("a node without a note", text)
	}
}
