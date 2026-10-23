package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestRecoverChildTargetsOnlyExactChildAndExplainsCapacity(t *testing.T) {
	calls := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/master/tasks/224/children/19/recover" || r.URL.RawQuery != "" {
			t.Fatalf("unexpected recovery target: %s %s", r.Method, r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"outcome": "WAITING_CAPACITY"}})
	})
	result, err := runTaskCommand(client, []string{"recover-child", "#224-19"})
	if err != nil || calls != 1 {
		t.Fatalf("recovery calls=%d error=%v", calls, err)
	}
	var output bytes.Buffer
	renderCLIResult(&output, result)
	if !strings.Contains(output.String(), "节点并发已满") || strings.Contains(output.String(), "子任务已恢复") {
		t.Fatalf("capacity incorrectly reported as started: %s", output.String())
	}
	for _, args := range [][]string{{}, {"224"}, {"224-0"}, {"224-1-2"}, {"224-#1"}, {"224-1", "225-1"}, {"224-19", "--watch", "1"}} {
		if _, err := runTaskCommand(client, append([]string{"recover-child"}, args...)); err == nil {
			t.Fatalf("invalid selection accepted: %v", args)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid selection sent %d extra mutations", calls-1)
	}
}
