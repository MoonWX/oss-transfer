package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// An exception list is read from the Master's own index; no node is asked.
// The nodes the Master names in failedWorkers and unreachableWorkers are
// offline with unfinished subtasks whose failures may not be reported yet:
// they neither failed nor left a request unanswered, and the CLI says what
// they are.
func TestExceptionListsNameOfflineNodesForWhatTheyAre(t *testing.T) {
	warning := "执行节点 agent-x (10.0.0.9) 离线，其未完成子任务的失败记录可能尚未上报，结果可能不完整。"
	offline := func(result map[string]interface{}) map[string]interface{} {
		result["queriedWorkers"] = []interface{}{"10.0.0.2", "10.0.0.9"}
		result["failedWorkers"], result["unreachableWorkers"] = []interface{}{"10.0.0.9"}, []interface{}{"10.0.0.9"}
		result["complete"], result["partial"], result["warning"] = false, true, warning
		displays := result["nodeDisplays"].(map[string]interface{})
		displays["10.0.0.9"] = map[string]interface{}{"displayLabel": "agent-x (10.0.0.9)", "displayName": "agent-x", "displayAddress": "10.0.0.9"}
		return result
	}
	failed := func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, offline(exceptionsFixture())) }
	local := func(w http.ResponseWriter, r *http.Request) {
		data := offline(map[string]interface{}{"items": []interface{}{}, "total": 0, "offset": 0, "limit": 25, "hasMore": false, "nodeDisplays": map[string]interface{}{}})
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": data})
	}
	for _, test := range []struct {
		args    []string
		handler http.HandlerFunc
		label   string
	}{
		{[]string{"exceptions", "list", "failed", "--task-id", "1"}, failed, "离线且有未完成子任务的节点: [agent-x (10.0.0.9)]"},
		{[]string{"exceptions", "export", "failed", "--task-id", "1", "--output", filepath.Join(t.TempDir(), "failed.csv")}, failed, "离线且有未完成子任务的节点: [agent-x (10.0.0.9)]"},
		{[]string{"exceptions", "list", "local", "--task-id", "1"}, local, "离线且有未完成子任务的节点: [agent-x (10.0.0.9)]"},
		{[]string{"--lang", "en", "exceptions", "list", "failed", "--task-id", "1"}, failed, "Offline nodes with unfinished subtasks: [agent-x (10.0.0.9)]"},
		{[]string{"--lang", "ja", "exceptions", "list", "failed", "--task-id", "1"}, failed, "未完了のサブタスクがあるオフラインのノード: [agent-x (10.0.0.9)]"},
	} {
		code, out, errout := runMockCLI(t, test.args, test.handler)
		if code != 3 || strings.Count(errout, test.label) != 1 || !strings.Contains(errout, warning) {
			t.Errorf("%v: exit %d\n%s\nstandard error:\n%s", test.args, code, out, errout)
		}
		for _, wrong := range []string{"失败节点", "未响应节点", "Failed nodes", "Unresponsive nodes", "失敗したノード", "応答しないノード"} {
			if strings.Contains(errout, wrong) {
				t.Errorf("%v calls the offline node %q:\n%s", test.args, wrong, errout)
			}
		}
	}
	// --json keeps the Master's fields and nothing of the CLI's own.
	code, out, _ := runMockCLI(t, []string{"--json", "exceptions", "list", "failed", "--task-id", "1"}, failed)
	var document map[string]interface{}
	if code != 3 || json.Unmarshal([]byte(out), &document) != nil || document[cliOfflineNodesKey] != nil || len(document["failedWorkers"].([]interface{})) != 1 {
		t.Fatalf("JSON: exit %d\n%s", code, out)
	}
}
