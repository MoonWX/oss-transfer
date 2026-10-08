package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestNodeTableDistinguishesExecutionSubtasksFromLegacyCounts(t *testing.T) {
	current := map[string]interface{}{
		"scope": "CURRENT_EXECUTION_SUBTASKS", "runningTasks": float64(2),
		"listDoneTasks": json.Number("3"), "pendingTasks": 4, "completedTasks": int64(7),
	}
	result := map[string]interface{}{
		"data": map[string]interface{}{"nodes": []interface{}{
			map[string]interface{}{"address": "10.0.0.2"},
			map[string]interface{}{"address": "10.0.0.3"},
			map[string]interface{}{"address": "10.0.0.4"},
		}},
		"workers": map[string]interface{}{
			"10.0.0.2": map[string]interface{}{"success": true, "executionTaskStats": current, "taskStats": map[string]interface{}{"completedTasks": 999}},
			"10.0.0.3": map[string]interface{}{"success": true, "taskStats": map[string]interface{}{"runningTasks": 6, "listDoneTasks": 100, "pendingTasks": 8, "completedTasks": 9}},
			"10.0.0.4": map[string]interface{}{"success": false, "executionTaskStats": current},
		},
	}
	before, _ := json.Marshal(result)
	var output bytes.Buffer
	renderCLIResult(&output, result)
	normalized := strings.Join(strings.Fields(output.String()), " ")
	for _, want := range []string{
		"统计口径 进行中 待执行 已完成",
		"10.0.0.2 执行节点 在线 当前子任务 5 4 7",
		"10.0.0.3 执行节点 在线 旧版记录 6 8 9",
		"10.0.0.4 执行节点 无法连接 — — — —",
		"按节点当前保留的子任务计数", "进行中包含扫描后传输", "node --help",
		"旧版记录沿用旧统计口径",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing %q in %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "999") {
		t.Fatal("legacy completed count replaced authoritative execution statistics")
	}
	after, _ := json.Marshal(result)
	if !bytes.Equal(before, after) {
		t.Fatal("rendering mutated original API fields")
	}
}

func TestNodeTaskCountsPreserveUnknownAndLegacySemantics(t *testing.T) {
	for _, invalid := range []interface{}{nil, "2", -1, 1.5, math.Inf(1), math.NaN()} {
		stats := map[string]interface{}{"scope": "CURRENT_EXECUTION_SUBTASKS", "runningTasks": 2, "listDoneTasks": invalid, "pendingTasks": invalid, "completedTasks": 0}
		scope, running, pending, completed := cliNodeTaskCounts(map[string]interface{}{"success": true, "executionTaskStats": stats, "taskStats": map[string]interface{}{"runningTasks": 99}})
		if scope != "当前子任务" || running != "—" || pending != "—" || completed != "0" {
			t.Fatalf("invalid %v became a known count: %s/%s/%s/%s", invalid, scope, running, pending, completed)
		}
	}
	scope, running, pending, completed := cliNodeTaskCounts(map[string]interface{}{"success": true, "executionTaskStats": map[string]interface{}{"scope": "FUTURE_UNKNOWN_SCOPE", "completedTasks": 99}, "taskStats": map[string]interface{}{"runningTasks": 1, "pendingTasks": 2, "completedTasks": 3}})
	if scope != "旧版记录" || running != "1" || pending != "2" || completed != "3" {
		t.Fatalf("unknown scope was labeled as current execution subtasks: %s/%s/%s/%s", scope, running, pending, completed)
	}
}

func TestNodeJSONRetainsBothStatisticsAndHelpExplainsScope(t *testing.T) {
	code, output, stderr := runMockCLI(t, []string{"node", "list", "--json"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/nodes":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2"}}}})
		case "/api/master/worker-status":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "workers": map[string]interface{}{"10.0.0.2": map[string]interface{}{"success": true, "taskStats": map[string]interface{}{"completedTasks": 99}, "executionTaskStats": map[string]interface{}{"scope": "CURRENT_EXECUTION_SUBTASKS", "runningTasks": 2, "listDoneTasks": 3, "completedTasks": 7}}}})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	})
	var result map[string]interface{}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(output), &result) != nil {
		t.Fatalf("node JSON failed: %d %s %s", code, output, stderr)
	}
	worker := result["workers"].(map[string]interface{})["10.0.0.2"].(map[string]interface{})
	legacy := worker["taskStats"].(map[string]interface{})
	current := worker["executionTaskStats"].(map[string]interface{})
	if legacy["completedTasks"] != float64(99) || current["completedTasks"] != float64(7) || current["runningTasks"] != float64(2) || current["listDoneTasks"] != float64(3) {
		t.Fatalf("JSON statistics changed: %s", output)
	}
	for _, phrase := range []string{"当前保留的已完成子任务", "不是累计完成的主任务数", "旧版记录"} {
		if !strings.Contains(helpFor([]string{"node"}), phrase) {
			t.Errorf("node help omitted %s", phrase)
		}
	}
}
