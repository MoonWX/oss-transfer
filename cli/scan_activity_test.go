package main

import (
	"bytes"
	"strings"
	"testing"
)

// What a scanner reading one large NFS directory reports: 86,980 names read,
// the directory's files not yet examined.
func readingActivity(extra map[string]interface{}) map[string]interface{} {
	activity := map[string]interface{}{
		"state": "LIVE", "observedAt": float64(1795395723000), "ageMillis": float64(0), "mode": "SORTED",
		"namesRead": float64(86980), "examined": float64(1200), "alreadyFound": float64(0), "vanished": float64(0),
		"directory":        map[string]interface{}{"key": "data/2026", "reading": true, "names": float64(86980), "position": float64(0), "elapsedMillis": float64(300000)},
		"lookupMeanMillis": nil, "oldestCallMillis": float64(0), "waitingFor": "", "idleMillis": float64(1000),
	}
	for key, value := range extra {
		activity[key] = value
	}
	return activity
}

func scanningTask(activity map[string]interface{}) map[string]interface{} {
	task := map[string]interface{}{"taskIndex": float64(61), "direction": "LOCAL_TO_OSS", "status": "RUNNING", "scanComplete": false,
		"listObjectCount": float64(0), "putObjectCount": float64(0), "skippedObjectCount": float64(0), "failedObjectCount": float64(0)}
	if activity != nil {
		task["scanActivity"] = activity
	}
	return task
}

// The task list's progress column moves while a directory is read, keeps a
// stale report's numbers with their age, and never shows numbers for a
// scanner that reports none.
func TestTaskListShowsTheScanWhileADirectoryIsRead(t *testing.T) {
	for _, test := range []struct {
		activity map[string]interface{}
		want     string
	}{
		{readingActivity(nil), "扫描中 · 已读取 86980 · 已检查 1200"},
		{readingActivity(map[string]interface{}{"state": "STALE", "ageMillis": float64(65000)}), "扫描中 · 已读取 86980 · 已检查 1200 · 1 分 5 秒前"},
		{map[string]interface{}{"state": "UNSUPPORTED"}, "扫描中"},
		{nil, "—"},
	} {
		var buffer bytes.Buffer
		renderTaskRows(&buffer, []map[string]interface{}{scanningTask(test.activity)}, false)
		lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], "已迁移/已发现") || !strings.Contains(lines[1], " "+test.want+" ") {
			t.Fatalf("want %q in:\n%s", test.want, buffer.String())
		}
	}
}

// task show and task nodes print what the scan is doing, in the console's
// wording and order.
func TestTaskShowAndNodesPrintTheScanProgress(t *testing.T) {
	var buffer bytes.Buffer
	renderTaskDetail(&buffer, scanningTask(readingActivity(nil)))
	for _, want := range []string{"已发现", "扫描进度\n",
		"  本轮扫描已读取 86980 个名称，已检查 1200 项（扫描重启后从零计）\n",
		"  正在读取目录“data/2026”中的名称：已读取 86980 个，用时 5 分 0 秒。读完全部名称后才会开始检查和迁移其中的文件。\n"} {
		if !strings.Contains(buffer.String(), want) {
			t.Fatalf("task show lacks %q:\n%s", want, buffer.String())
		}
	}
	examining := readingActivity(map[string]interface{}{
		"alreadyFound": float64(3), "vanished": float64(1), "lookupMeanMillis": 23.4, "oldestCallMillis": float64(6000),
		"waitingFor": "LOCAL_OPERATION_LIMIT", "idleMillis": float64(40000),
		"directory": map[string]interface{}{"key": "", "reading": false, "names": float64(86980), "position": float64(1201), "elapsedMillis": float64(1)},
	})
	buffer.Reset()
	renderTaskNodeNotes(&buffer, map[string]interface{}{"scan": map[string]interface{}{"phase": "SCANNING", "activity": examining}})
	want := "扫描进度\n" +
		"  本轮扫描已读取 86980 个名称，已检查 1200 项（扫描重启后从零计）\n" +
		"  正在检查源目录：第 1201 项，共 86980 项\n" +
		"  其中 3 项此前已发现，未再次检查\n" +
		"  1 项在检查前已被删除，已跳过\n" +
		"  条目查询平均耗时 23 毫秒\n" +
		"  源存储已有 6 秒 未响应\n" +
		"  正在等待本节点的本地文件操作限制\n"
	if buffer.String() != want {
		t.Fatalf("task nodes:\n%s\nwant:\n%s", buffer.String(), want)
	}
	buffer.Reset()
	idle := readingActivity(map[string]interface{}{"state": "STALE", "idleMillis": float64(40000)})
	renderTaskNodeNotes(&buffer, map[string]interface{}{"scan": map[string]interface{}{"phase": "SCANNING", "activity": idle}})
	if !strings.Contains(buffer.String(), "  已有 40 秒 没有新进展\n") || !strings.Contains(buffer.String(), "  扫描进度截至 ") {
		t.Fatalf("idle and stale lines:\n%s", buffer.String())
	}
	buffer.Reset()
	streaming := readingActivity(map[string]interface{}{"mode": "STREAMING",
		"directory": map[string]interface{}{"key": "data", "reading": true, "names": float64(5000), "position": float64(4001), "examined": float64(3998), "elapsedMillis": float64(61000)}})
	renderTaskNodeNotes(&buffer, map[string]interface{}{"scan": map[string]interface{}{"phase": "SCANNING", "activity": streaming}})
	if !strings.Contains(buffer.String(), "  正在读取并检查目录“data”：已读取 5000 个名称，已检查 3998 项，用时 1 分 1 秒\n") {
		t.Fatalf("streamed directory:\n%s", buffer.String())
	}
	buffer.Reset()
	renderTaskNodeNotes(&buffer, map[string]interface{}{"scan": map[string]interface{}{"phase": "SCANNING", "activity": map[string]interface{}{"state": "UNSUPPORTED"}}})
	if buffer.String() != "扫描进度\n  扫描节点版本较旧，不提供实时扫描进度\n" {
		t.Fatalf("older scanner:\n%s", buffer.String())
	}
	buffer.Reset()
	renderTaskNodeNotes(&buffer, map[string]interface{}{"scan": map[string]interface{}{"phase": "WAITING", "upgradeRequired": true}})
	if buffer.String() != "其余节点版本较旧，无法接续此扫描；任一节点升级后自动继续。\n" {
		t.Fatalf("waiting for an upgrade:\n%s", buffer.String())
	}
	// Without scan activity, nothing is added.
	buffer.Reset()
	renderTaskDetail(&buffer, scanningTask(nil))
	if strings.Contains(buffer.String(), "扫描进度") {
		t.Fatalf("scan progress printed without a report:\n%s", buffer.String())
	}
}

func TestScanProgressReadsNaturallyInEnglishAndJapanese(t *testing.T) {
	one := readingActivity(map[string]interface{}{"namesRead": float64(1), "examined": float64(1), "alreadyFound": float64(1), "vanished": float64(1),
		"directory": map[string]interface{}{"key": "", "reading": true, "names": float64(1), "position": float64(0), "elapsedMillis": float64(1000)}})
	for _, test := range []struct {
		locale cliLocale
		want   []string
	}{
		{localeEN, []string{"Found", "Scan progress\n", "This scan run has read 1 name and examined 1 entry (counted from zero after a restart)",
			"Reading the names in the source directory: 1 read in 1 s.", "Already found and not examined again: 1", "Deleted before examination and skipped: 1"}},
		{localeJA, []string{"発見済み", "スキャンの進捗\n", "今回のスキャンで名前を 1 件読み取り、1 件を確認しました", "転送元ディレクトリの名前を読み取っています"}},
	} {
		var buffer bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: test.locale}, scanningTask(one))
		for _, want := range test.want {
			if !strings.Contains(buffer.String(), want) {
				t.Fatalf("%s lacks %q:\n%s", test.locale, want, buffer.String())
			}
		}
	}
	var buffer bytes.Buffer
	renderTaskRows(&cliLocaleWriter{Writer: &buffer, locale: localeEN}, []map[string]interface{}{scanningTask(readingActivity(nil))}, false)
	if !strings.Contains(buffer.String(), "Scanning · 86980 read · 1200 examined") || !strings.Contains(buffer.String(), "Transferred/Found") {
		t.Fatalf("English task list:\n%s", buffer.String())
	}
}
