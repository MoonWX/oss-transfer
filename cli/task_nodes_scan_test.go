package main

import (
	"bytes"
	"strings"
	"testing"
)

// The node view names the scan state instead of tagging a member once the scan
// no longer has a running scanner, and explains why an offline leaver waits.
func TestTaskNodesShowsScanPhaseAndLeaseExpiry(t *testing.T) {
	for phase, want := range map[string][2]string{
		"COMPLETE": {"扫描已完成，不再需要扫描节点。", "The scan is complete; no scanning node is needed."},
		"HANDOFF":  {"扫描正在移交给新的扫描节点，从 Master 保存的断点继续。", "continues from the Master's checkpoint"},
		"WAITING":  {"原扫描节点已退出，其他节点将从 Master 保存的断点接管扫描。", "another node takes over the scan"},
	} {
		view := taskNodesTestView(false)
		view["scan"] = map[string]interface{}{"phase": phase, "complete": phase == "COMPLETE"}
		leaver := view["nodes"].([]interface{})[1].(map[string]interface{})
		leaver["online"], leaver["progress"] = false, "LEASE_EXPIRY"
		master := &taskNodesFakeMaster{view: view}
		result, err := runTaskCommand(adminTestClient(t, master.handler(t)), []string{"nodes", "12"})
		if err != nil {
			t.Fatal(err)
		}
		chinese, english := renderTaskNodesForTest(result, localeZH), renderTaskNodesForTest(result, localeEN)
		if !strings.Contains(chinese, want[0]) || !strings.Contains(english, want[1]) {
			t.Errorf("%s: scan note missing:\n%s\n%s", phase, chinese, english)
		}
		if !strings.Contains(chinese, "，离线，执行租约到期后离开") || !strings.Contains(english, "leaves when its execution lease expires") {
			t.Errorf("%s: lease-expiry progress missing:\n%s\n%s", phase, chinese, english)
		}
	}
	// Task rows carry the phase as scanPhase; a running scan adds no note.
	for phase, want := range map[string]string{"COMPLETE": "扫描已完成", "SCANNING": ""} {
		var buffer bytes.Buffer
		renderTaskNodeNotes(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, map[string]interface{}{"scanPhase": phase})
		if got := strings.TrimSpace(buffer.String()); !strings.Contains(got, want) || want == "" && got != "" {
			t.Fatalf("%s: %q", phase, got)
		}
	}
}
