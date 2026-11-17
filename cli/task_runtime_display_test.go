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

// A node that did not answer is named, and a partial reply without files is
// not also reported as having no file transfers at all.
func TestRuntimeProgressNamesNodesThatDidNotAnswer(t *testing.T) {
	silent := []interface{}{map[string]interface{}{"displayLabel": "worker-b (10.0.1.13)", "executionNodeIp": "10.0.1.13"}}
	for _, tc := range []struct {
		data        map[string]interface{}
		want, avoid []string
	}{
		{map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "partial": true, "unreportedNodes": silent, "items": []interface{}{}},
			[]string{"执行节点 worker-b (10.0.1.13) 未响应，其文件进度暂不可用。"}, []string{"当前没有可显示的文件传输进度", "请稍后刷新"}},
		{map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "partial": true, "items": []interface{}{}},
			[]string{"部分文件进度暂不可用，请稍后刷新。"}, []string{"当前没有可显示的文件传输进度"}},
		{map[string]interface{}{"parentTaskId": 1, "observedAt": "now", "items": []interface{}{}},
			[]string{"当前没有可显示的文件传输进度。"}, []string{"暂不可用"}},
	} {
		var out bytes.Buffer
		if !renderTaskRuntimeResult(&out, map[string]interface{}{"data": tc.data}) {
			t.Fatal("progress was not rendered")
		}
		for _, want := range tc.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q in %s", want, out.String())
			}
		}
		for _, avoid := range tc.avoid {
			if strings.Contains(out.String(), avoid) {
				t.Errorf("contains %q: %s", avoid, out.String())
			}
		}
	}
	task := map[string]interface{}{"taskIndex": 61, "status": "RUNNING", "direction": "OSS_TO_OSS", "scanComplete": true, "listObjectCount": 10, "listObjectSize": 1 << 30, "putObjectCount": 4, "transferredBytes": 1 << 29, "progressPercent": 50, "liveProgress": "PARTIAL",
		"unreportedNodes": append(silent, map[string]interface{}{"displayLabel": "worker-c (10.0.1.14)", "executionNodeIp": "10.0.1.14"})}
	// Names are separated as each language separates a list.
	for _, test := range []struct {
		locale cliLocale
		want   string
	}{
		{localeZH, "执行节点 worker-b (10.0.1.13)、worker-c (10.0.1.14) 未上报，已迁移和进度不含其正在传输的部分，速度暂时未知"},
		{localeEN, "No live progress from worker-b (10.0.1.13), worker-c (10.0.1.14)"},
		{localeJA, "worker-b (10.0.1.13)、worker-c (10.0.1.14)"},
	} {
		var out bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &out, locale: test.locale}, task)
		if !strings.Contains(out.String(), test.want) {
			t.Errorf("%s: task detail lacks %q:\n%s", test.locale, test.want, out.String())
		}
	}
}
