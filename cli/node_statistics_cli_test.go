package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNodeTableOmitsExecutionCountsAndPreservesRawStatistics(t *testing.T) {
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
		"节点 节点标识 角色 连接状态",
		"10.0.0.2 10.0.0.2 执行节点 在线",
		"10.0.0.3 10.0.0.3 执行节点 在线",
		"10.0.0.4 10.0.0.4 执行节点 无法连接",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing %q in %s", want, output.String())
		}
	}
	for _, unwanted := range []string{"统计口径", "进行中", "待执行", "已完成", "当前子任务", "旧版记录", "999"} {
		if strings.Contains(output.String(), unwanted) {
			t.Errorf("node table still displays %q: %s", unwanted, output.String())
		}
	}
	after, _ := json.Marshal(result)
	if !bytes.Equal(before, after) {
		t.Fatal("rendering mutated original API fields")
	}
}

func TestNodeJSONRetainsBothStatisticsAndHelpOmitsUnusedCounts(t *testing.T) {
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
		if strings.Contains(helpFor([]string{"node"}), phrase) {
			t.Errorf("node help still explains hidden count %s", phrase)
		}
	}
}

func TestWorkerStatusAliasOmitsTaskCounters(t *testing.T) {
	code, output, stderr := runMockCLI(t, []string{"worker-status"}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/nodes":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2", "label": "Agent 2", "agent": true, "online": true}}}})
		case "/api/master/worker-status":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "workers": map[string]interface{}{"10.0.0.2": map[string]interface{}{"success": true, "executionTaskStats": map[string]interface{}{"scope": "CURRENT_EXECUTION_SUBTASKS", "completedTasks": 999}}}})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	})
	if code != 0 || stderr != "" || !strings.Contains(output, "Agent 2") || !strings.Contains(output, "在线") {
		t.Fatalf("worker-status alias failed: %d %s %s", code, output, stderr)
	}
	for _, unwanted := range []string{"999", "已完成", "待执行", "进行中", "统计口径"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("worker-status still displays %q: %s", unwanted, output)
		}
	}
}
