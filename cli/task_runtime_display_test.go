package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestRuntimeProgressDistinguishesMissingAndPartialData(t *testing.T) {
	for _, tc := range []struct {
		data map[string]interface{}
		want string
	}{
		{map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "supported": false}, "不支持文件进度"},
		{map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "partial": true, "items": []interface{}{}}, "部分文件进度暂不可用"},
	} {
		var out bytes.Buffer
		if !renderTaskRuntimeResult(&out, map[string]interface{}{"data": tc.data}) || !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "0%") {
			t.Fatal(out.String())
		}
	}
	var out bytes.Buffer
	result := map[string]interface{}{"data": map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "items": []interface{}{map[string]interface{}{"childTaskId": "1-2", "executionNodeLabel": "storage (10.0.0.1)", "sourceKey": "line\n\x1b[31mfile", "size": float64(1024), "transferredBytes": float64(512), "stage": "TRANSFER"}, map[string]interface{}{"childTaskId": "1-3", "size": math.NaN(), "transferredBytes": float64(20)}}}}
	if !renderTaskRuntimeResult(&out, result) {
		t.Fatal("progress was not rendered")
	}
	for _, want := range []string{"#1-2", "storage (10.0.0.1)", "50.0%", "512 B / 1 KiB", "—"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "NaN%") {
		t.Fatal(out.String())
	}
}
func TestCleanupStatusDoesNotClaimCompletionForPendingRecords(t *testing.T) {
	var out bytes.Buffer
	result := map[string]interface{}{"cleanupStatus": true, "tasks": []interface{}{map[string]interface{}{"taskId": 7, "purged": true}, map[string]interface{}{"taskId": 8, "missing": true}, map[string]interface{}{"taskId": 9, "purged": false}}}
	if !renderTaskRuntimeResult(&out, result) {
		t.Fatal("cleanup not handled")
	}
	for _, want := range []string{"#7: 已永久删除", "#8: 未找到待清理记录", "#9: 等待清理"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
}
