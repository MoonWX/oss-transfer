package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// taskReportArchive builds a task report ZIP whose report-metadata.json is
// meta (no metadata when meta is nil).
func taskReportArchive(t *testing.T, meta map[string]interface{}) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	files := map[string]string{"task-summary.csv": "rowType\r\n", "file-results.csv": "parentTaskId\r\n", "exception-details.csv": "parentTaskId\r\n"}
	if meta != nil {
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		files["report-metadata.json"] = string(data)
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func taskReportServer(t *testing.T, path string, archive []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return server
}

func incompleteTaskReportMeta() map[string]interface{} {
	counts := map[string]interface{}{"SUCCESS": 128, "SKIPPED": 0, "FAILED": 0, "UNPROCESSED": 0, "UNKNOWN": 1896}
	reasons := []interface{}{"NODE_REPORT_INCOMPLETE", "COUNTS_DO_NOT_RECONCILE"}
	return map[string]interface{}{
		"schemaVersion": 4, "complete": false, "parentTaskId": 1, "resultCounts": counts, "partialReasons": reasons,
		"unreachableNodes": []interface{}{"10.0.0.5"},
		"nodeDisplays":     map[string]interface{}{"10.0.0.5": map[string]interface{}{"displayLabel": "worker-a (10.0.0.5)"}},
		"tasks":            []interface{}{map[string]interface{}{"parentTaskId": 1, "complete": false, "resultCounts": counts, "partialReasons": reasons}},
	}
}

func TestTaskReportExplainsIncompleteReportAndExitsThree(t *testing.T) {
	server := taskReportServer(t, "/api/master/task-reports/1", taskReportArchive(t, incompleteTaskReportMeta()))
	directory := t.TempDir()
	code, out, stderr := runComparisonCLI(t, server, "task", "report", "1", "--output", filepath.Join(directory, "zh.zip"))
	for _, want := range []string{"任务报告已保存", "报告完整", "否", "成功 128，跳过 0，失败 0，未处理 0，结果未知 1896", "不完整原因", "部分对象的结果无法从执行节点读取（worker-a (10.0.0.5)）", "报告中的对象数与任务统计不一致", "exception-details.csv"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, code := range []string{"NODE_REPORT_INCOMPLETE", "COUNTS_DO_NOT_RECONCILE", "LEDGER_UNAVAILABLE", "e+"} {
		if strings.Contains(out, code) {
			t.Errorf("human output shows internal code or scientific notation %q:\n%s", code, out)
		}
	}
	if code != 3 || !strings.Contains(stderr, "返回不完整") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(directory, "zh.zip")); err != nil {
		t.Fatal("an incomplete report must still be saved", err)
	}

	for _, test := range []struct{ lang, want string }{
		{"en", "Some object results could not be read from the execution nodes (worker-a (10.0.0.5))"},
		{"ja", "一部のオブジェクトの結果を実行ノードから読み取れませんでした（worker-a (10.0.0.5)）"},
	} {
		code, out, _ := runComparisonCLI(t, server, "--lang", test.lang, "task", "report", "1", "--output", filepath.Join(directory, test.lang+".zip"))
		if code != 3 || !strings.Contains(out, test.want) {
			t.Errorf("%s: exit %d:\n%s", test.lang, code, out)
		}
	}

	// Machine output keeps the Master's codes.
	code, out, _ = runComparisonCLI(t, server, "--json", "task", "report", "1", "--output", filepath.Join(directory, "json.zip"))
	var result map[string]interface{}
	if code != 3 || json.Unmarshal([]byte(out), &result) != nil {
		t.Fatalf("exit %d: %s", code, out)
	}
	reasons := cliStringList(result["incompleteReasons"])
	counts, _ := result["resultCounts"].(map[string]interface{})
	if result["complete"] != false || strings.Join(reasons, ",") != "NODE_REPORT_INCOMPLETE,COUNTS_DO_NOT_RECONCILE" || counts["UNKNOWN"] != 1896.0 || counts["SUCCESS"] != 128.0 {
		t.Fatalf("bad JSON result: %s", out)
	}
}

func TestTaskReportCompleteExitsZero(t *testing.T) {
	counts := map[string]interface{}{"SUCCESS": 2053, "SKIPPED": 0, "FAILED": 0, "UNPROCESSED": 0, "UNKNOWN": 0}
	meta := map[string]interface{}{"complete": true, "resultCounts": counts, "partialReasons": []interface{}{}, "unreachableNodes": []interface{}{}, "tasks": []interface{}{map[string]interface{}{"complete": true, "resultCounts": counts, "partialReasons": []interface{}{}}}}
	server := taskReportServer(t, "/api/master/task-reports/9", taskReportArchive(t, meta))
	code, out, stderr := runComparisonCLI(t, server, "task", "report", "9", "--output", filepath.Join(t.TempDir(), "r.zip"))
	if code != 0 || stderr != "" || !strings.Contains(out, "报告完整") || !strings.Contains(out, "是") || !strings.Contains(out, "成功 2053") || strings.Contains(out, "不完整原因") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
}

// The local transfer report sums its tasks and names every reason once.
func TestLocalTransferReportReportsIncompleteTasks(t *testing.T) {
	meta := map[string]interface{}{"complete": false, "unreachableNodes": []interface{}{}, "tasks": []interface{}{
		map[string]interface{}{"complete": true, "resultCounts": map[string]interface{}{"SUCCESS": 3}, "partialReasons": []interface{}{}},
		map[string]interface{}{"complete": false, "resultCounts": map[string]interface{}{"SUCCESS": 1, "UNPROCESSED": 2}, "partialReasons": []interface{}{"TASK_STOPPED", "UNPROCESSED_OBJECTS"}},
	}}
	server := taskReportServer(t, "/api/master/local-transfers/report", taskReportArchive(t, meta))
	code, out, _ := runComparisonCLI(t, server, "exceptions", "export", "local", "--output", filepath.Join(t.TempDir(), "local.zip"))
	for _, want := range []string{"本地迁移报告已保存", "成功 4，跳过 0，失败 0，未处理 2，结果未知 0", "任务已停止，还有对象没有迁移", "有对象尚未处理"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code != 3 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// A Master that predates report metadata still yields a saved report.
func TestTaskReportWithoutMetadataStillSucceeds(t *testing.T) {
	server := taskReportServer(t, "/api/master/task-reports/4", taskReportArchive(t, nil))
	code, out, stderr := runComparisonCLI(t, server, "task", "report", "4", "--output", filepath.Join(t.TempDir(), "r.zip"))
	if code != 0 || stderr != "" || !strings.Contains(out, "的报告已保存到") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
}

// An incomplete report written to standard output is still written whole;
// the reason goes to standard error and the exit status is 3.
func TestIncompleteTaskReportToStandardOutputExitsThree(t *testing.T) {
	archive := taskReportArchive(t, map[string]interface{}{"complete": false, "unreachableNodes": []interface{}{"10.0.0.9"}, "tasks": []interface{}{
		map[string]interface{}{"parentTaskId": 4, "complete": false, "partialReasons": []interface{}{"NODE_REPORT_INCOMPLETE"}},
	}})
	server := taskReportServer(t, "/api/master/task-reports/4", archive)
	code, out, stderr := runComparisonCLI(t, server, "task", "report", "4", "--output", "-")
	if code != 3 || out != string(archive) {
		t.Fatalf("exit %d, %d of %d bytes on stdout: %s", code, len(out), len(archive), stderr)
	}
	if !strings.Contains(stderr, "报告不完整") || !strings.Contains(stderr, "10.0.0.9") {
		t.Fatalf("reason missing: %s", stderr)
	}
}
