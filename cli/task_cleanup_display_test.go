package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCLIPurgeReportsBackgroundAdmissionAndPreservesJSON(t *testing.T) {
	cleanup := map[string]interface{}{"status": "QUEUED", "phase": "AGENTS", "recordsDeleted": 0, "completedNodes": 0, "totalNodes": 2}
	result := map[string]interface{}{"success": true, "accepted": 1, "failed": 0, "ignored": 0, "details": []interface{}{map[string]interface{}{"taskId": 224, "success": true, "accepted": true, "cleanup": cleanup}}}
	for _, language := range []struct{ locale, want string }{{"zh", "清理请求已受理，将在后台继续"}, {"en", "Cleanup accepted and continuing in the background"}, {"ja", "クリーンアップを受理しました"}} {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/api/master/permanent-delete" {
				t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			}
			var body struct {
				TaskIDs []int `json:"taskIds"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.TaskIDs) != 1 || body.TaskIDs[0] != 224 {
				t.Fatal("wrong selected IDs")
			}
			adminTestJSON(t, w, result)
		}
		code, output, stderr := runMockCLI(t, []string{"--lang", language.locale, "task", "purge", "224", "--yes"}, handler)
		if code != 0 || stderr != "" || !strings.Contains(output, language.want) || !strings.Contains(output, "#224") || !strings.Contains(output, "task trash") {
			t.Fatalf("%s: %d %s %s", language.locale, code, output, stderr)
		}
		code, output, stderr = runMockCLI(t, []string{"--lang", language.locale, "task", "purge", "224", "--yes", "--json"}, handler)
		var raw map[string]interface{}
		if code != 0 || stderr != "" || json.Unmarshal([]byte(output), &raw) != nil {
			t.Fatalf("json: %d %s %s", code, output, stderr)
		}
		rows := objectList(raw["details"])
		if len(rows) != 1 || taskCleanup(rows[0])["status"] != "QUEUED" || raw["accepted"] != float64(1) {
			t.Fatalf("lost cleanup result: %#v", raw)
		}
	}
}

func TestCLITrashShowsCleanupCountsAndWaitingReason(t *testing.T) {
	result := cliTrashFixture()
	task := objectList(result["deletedTasks"].(map[string]interface{})["10.0.0.2"])[0]
	task["cleanup"] = map[string]interface{}{"status": "WAITING", "phase": "AGENTS", "recordsDeleted": 65536, "completedNodes": 1, "totalNodes": 2, "error": "Node offline"}
	for _, language := range []struct{ locale, label, count string }{{"zh", "等待自动继续", "已清理 65536 条记录"}, {"en", "Waiting to continue", "65536 records removed"}, {"ja", "自動再開を待機中", "65536 件の記録を削除済み"}} {
		code, output, stderr := runMockCLI(t, []string{"--lang", language.locale, "task", "trash"}, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Fatal("listing cleanup must not resubmit deletion")
			}
			adminTestJSON(t, w, result)
		})
		if code != 0 || stderr != "" || !strings.Contains(output, language.label) || !strings.Contains(output, language.count) || !strings.Contains(output, "1/2") || !strings.Contains(output, "Node offline") {
			t.Fatalf("%s: %d %s %s", language.locale, code, output, stderr)
		}
	}
}

func TestCLIAlreadyCleanedTaskDoesNotClaimBackgroundWork(t *testing.T) {
	result := map[string]interface{}{"success": true, "accepted": 0, "ignored": 1, "failed": 0, "details": []interface{}{map[string]interface{}{"taskId": 224, "success": true, "ignored": true, "purged": true, "cleanup": map[string]interface{}{"status": "DONE", "phase": "MASTER", "recordsDeleted": 66027, "completedNodes": 2, "totalNodes": 2}}}}
	code, output, stderr := runMockCLI(t, []string{"task", "purge", "224", "--yes"}, func(w http.ResponseWriter, _ *http.Request) { adminTestJSON(t, w, result) })
	if code != 0 || stderr != "" || !strings.Contains(output, "#224: 已清理") || strings.Contains(output, "后台继续") {
		t.Fatalf("wrong completed cleanup: %d %s %s", code, output, stderr)
	}
}
