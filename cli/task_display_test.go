package main

import (
	"bytes"
	"math"
	"runtime"
	"strings"
	"testing"
)

func TestTaskDisplayDistinguishesMissingMetricsFromActualZero(t *testing.T) {
	for _, unknown := range []interface{}{nil, "0", -1, math.NaN(), math.Inf(1)} {
		task := map[string]interface{}{"listObjectCount": unknown, "listObjectSize": unknown, "currentSpeedBytesPerSecond": unknown}
		if cliTaskMetric(task, "listObject") != "— / —" || cliTaskSpeed(task, "currentSpeedBytesPerSecond") != "—" {
			t.Fatalf("missing or invalid metrics became zero: %#v", task)
		}
	}
	zero := map[string]interface{}{"listObjectCount": 0, "listObjectSize": 0, "currentSpeedBytesPerSecond": 0}
	if cliTaskMetric(zero, "listObject") != "0 / 0 B" || cliTaskSpeed(zero, "currentSpeedBytesPerSecond") != "0 B/s" {
		t.Fatal("actual zero hidden")
	}
	var output bytes.Buffer
	renderTaskRows(&output, []map[string]interface{}{{"taskIndex": 42, "status": "PENDING"}}, false)
	if !strings.Contains(output.String(), "—/—") || strings.Contains(output.String(), "0/0") {
		t.Fatalf("unknown list metrics displayed as zero: %s", output.String())
	}
}

func TestTaskDetailsExposeMappingFiltersAndFailureReason(t *testing.T) {
	task := map[string]interface{}{
		"taskIndex": 42, "direction": "OSS_TO_OSS", "status": "FAILED", "createTime": float64(1789444800000),
		"objectKey": "source/a.txt", "keyMapping": map[string]interface{}{"mode": "EXACT_KEY", "targetKey": "archive/a.txt"},
		"dateAfter": "2026-09-01T00:00:00", "dateBefore": "2026-09-30T00:00:00",
		"errorCode": "PATH_MAPPING", "errorMessage": "目标路径冲突\n不可迁移", "failedObjectCount": 2, "failedObjectSize": 2048,
	}
	var output bytes.Buffer
	renderTaskDetail(&output, task)
	for _, want := range []string{"源对象 Key", "source/a.txt", "目标命名", "EXACT_KEY", "目标对象 Key", "archive/a.txt", "修改时间晚于", "2026-09-01T00:00:00", "修改时间早于", "PATH_MAPPING", "失败原因", "目标路径冲突 不可迁移", "2 / 2 KiB", "2026-"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "1789444800000") {
		t.Fatal("epoch timestamp left unformatted")
	}
	fields := cliTaskScopeFields(map[string]interface{}{"keyMapping": map[string]interface{}{"mode": "REPLACE_PREFIX", "sourcePrefix": "old/", "targetPrefix": ""}})
	if len(fields) != 3 || fields[2][1] != `""` {
		t.Fatalf("intentional empty replacement prefix lost: %#v", fields)
	}
}

func TestCLIBinaryUnitsMatchConsole(t *testing.T) {
	for bytes, want := range map[int64]string{0: "0 B", 24: "24 B", 1024: "1 KiB", 1536: "1.5 KiB", 1024 * 1024: "1 MiB", 136 * 1024 * 1024: "136 MiB", 1 << 40: "1 TiB"} {
		if actual := formatSize(bytes); actual != want {
			t.Errorf("%d bytes: %s, want %s", bytes, actual, want)
		}
	}
}

func TestTaskDisplayGroupsLegacyStatesWithoutRewritingJSON(t *testing.T) {
	for state, want := range map[string]string{"PENDING": "WAITING", "WAITING": "WAITING", "LIST_DONE": "RUNNING", "RUNNING": "RUNNING", "RECOVERABLE": "RECOVERABLE"} {
		row := map[string]interface{}{"taskIndex": 1, "status": state}
		var output bytes.Buffer
		renderTaskRows(&output, []map[string]interface{}{row}, false)
		if !strings.Contains(output.String(), want) || row["status"] != state {
			t.Fatalf("wrong display or mutated data: %s %#v", output.String(), row)
		}
	}
}

func TestSubtaskPriorityPageShowsActualRowsAndCursorCommand(t *testing.T) {
	for _, active := range []bool{false, true} {
		page := map[string]interface{}{"parentTaskId": 224, "totalCount": 80, "offset": 25, "limit": 25, "order": "priority", "nextCursor": "0/0000000004", "hasMore": true}
		if active {
			page["view"] = "active"
		}
		result := map[string]interface{}{"subtaskPage": page, "children": []map[string]interface{}{{"childTaskId": "224-99"}, {"childTaskId": "224-4"}}}
		var output bytes.Buffer
		renderSubtaskPage(&output, result)
		text := output.String()
		if !strings.Contains(text, "本页 2 个子任务") || !strings.Contains(text, "task children 224 --cursor '0/0000000004' --limit 25 --order priority") || strings.Contains(text, "--offset") || strings.Contains(text, "子任务编号") {
			t.Fatal("priority order was displayed as ordinal pagination", text)
		}
		if strings.Contains(text, "--active") != active {
			t.Fatal("next page changed active/all view", text)
		}
	}
}

func TestSubtaskPriorityEmptyAndIndexingPagesDoNotInventNumberWindows(t *testing.T) {
	for _, indexing := range []bool{false, true} {
		result := map[string]interface{}{"children": []interface{}{}, "subtaskPage": map[string]interface{}{"order": "priority", "totalCount": 999, "limit": 25, "indexing": indexing}}
		var output bytes.Buffer
		renderSubtaskPage(&output, result)
		text := output.String()
		if indexing && !strings.Contains(text, "正在整理子任务，请稍后重试。") || !indexing && !strings.Contains(text, "本页 0 个子任务") || strings.Contains(text, "编号") || strings.Contains(text, "下一页") {
			t.Fatal("empty/indexing page claimed a numbered window", text)
		}
	}
}

func TestSubtaskNextCursorIsQuotedAsOneShellArgument(t *testing.T) {
	cursor := "0/0000000004'; $(echo injected); '"
	want := "'0/0000000004'\\''; $(echo injected); '\\'''"
	if runtime.GOOS == "windows" {
		want = "'0/0000000004''; $(echo injected); '''"
	}
	if got := quoteTaskCursor(cursor); got != want {
		t.Fatalf("unsafe cursor argument: %q, want %q", got, want)
	}
	var output bytes.Buffer
	renderSubtaskPage(&output, map[string]interface{}{"children": []interface{}{}, "subtaskPage": map[string]interface{}{"parentTaskId": 1, "order": "priority", "limit": 25, "hasMore": true, "nextCursor": cursor}})
	if !strings.Contains(output.String(), "--cursor "+want+" --limit") {
		t.Fatal("next-page command did not use the quoted cursor", output.String())
	}
}

func TestSubtaskNumberPagePreservesExplicitOrderAndActiveView(t *testing.T) {
	var output bytes.Buffer
	renderSubtaskPage(&output, map[string]interface{}{"children": []map[string]interface{}{{"childTaskId": "224-26"}}, "subtaskPage": map[string]interface{}{"parentTaskId": 224, "totalCount": 80, "offset": 25, "limit": 25, "order": "id", "view": "active", "hasMore": true}})
	text := output.String()
	if !strings.Contains(text, "子任务编号 26–50 / 80") || !strings.Contains(text, "task children 224 --offset 50 --limit 25 --order id --active") || strings.Contains(text, "--cursor") {
		t.Fatal("explicit numeric page changed behavior", text)
	}
}
