package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// comparisonTestPage is a Master comparison page as JSON decodes it.
func comparisonTestPage(overrides map[string]interface{}) map[string]interface{} {
	page := map[string]interface{}{
		"id": "job-1", "status": "COMPLETED", "mode": "metadata", "scope": "prefix",
		"source":       map[string]interface{}{"kind": "S3", "ref": "src", "bucket": "photos"},
		"destination":  map[string]interface{}{"kind": "S3", "ref": "dst", "bucket": "archive"},
		"sourcePrefix": "2026/", "destinationPrefix": "backup/",
		"compared": 1200, "matched": 1100, "different": 40, "etagMismatch": 10, "missing": 30, "extra": 10,
		"changed": 5, "errors": 4, "bothMissing": 1, "rows": 100, "rowsTruncated": false, "resultLimit": 0,
		"storedBytes": 25600, "bytesRead": 0, "elapsedMillis": 60000, "resumable": false,
		"startedAt": "2026-11-09T12:00:00Z", "updatedAt": "2026-11-09T12:01:00Z", "endedAt": "2026-11-09T12:01:00Z",
		"items": []interface{}{},
	}
	for key, value := range overrides {
		if value == nil {
			delete(page, key)
		} else {
			page[key] = value
		}
	}
	return page
}

func comparisonTestRows() []interface{} {
	return []interface{}{
		map[string]interface{}{"number": 1, "key": "a.jpg", "sourceKey": "2026/a.jpg", "destinationKey": "backup/a.jpg", "status": "DIFFERENT", "sourceSize": 2048, "destinationSize": 1024, "reasons": []interface{}{"size", "contentType"}},
		map[string]interface{}{"number": 2, "key": "b.jpg", "sourceKey": "2026/b.jpg", "destinationKey": "backup/b.jpg", "status": "ETAG_MISMATCH", "sourceSize": 8, "destinationSize": 8, "reasons": []interface{}{"etag"}},
		map[string]interface{}{"number": 3, "key": "c\tjpg", "sourceKey": "2026/c\tjpg", "destinationKey": "backup/c\tjpg", "status": "MISSING_TARGET", "sourceSize": 4},
		map[string]interface{}{"number": 4, "key": "d.jpg", "sourceKey": "2026/d.jpg", "destinationKey": "backup/d.jpg", "status": "ERROR", "error": "Cannot read target metadata", "errorDetail": "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)"},
	}
}

func comparisonUnexpectedRequest(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid command reached the server: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusTeapot)
	}
}

func TestComparisonCommandLineErrorsExitTwoWithoutRequests(t *testing.T) {
	directory := t.TempDir()
	for _, args := range [][]string{
		{"object", "status", "job-1", "--limit", "0"},
		{"object", "status", "job-1", "--limit", "1001"},
		{"object", "status", "job-1", "--after", "-1"},
		{"object", "status", "job-1", "--after", "x"},
		{"object", "status", "job-1", "--status", "MATCH"},
		{"object", "status", "job-1", "--status", "DIFFERENT,"},
		{"object", "status", "job-1", "--status", "bogus"},
		{"object", "status", "job-1", "--status", "DIFFERENT", "--status", "ERROR"},
		{"object", "status"},
		{"object", "status", "job-1", "job-2"},
		{"object", "status", "a/b"},
		{"object", "status", "job-1", "--bogus", "1"},
		{"object", "list", "job-1"},
		{"object", "export", "job-1"},
		{"object", "export", "job-1", "--output", filepath.Join(directory, "x.csv"), "--format", "csv"},
		{"object", "export", "job-1", "--output", filepath.Join(directory, "x.zip"), "--status", "SAME"},
		{"object", "resume"},
		{"object", "cancel", "job-1", "--after", "1"},
		{"object", "delete", ""},
		{"object", "frobnicate"},
		{"object", "compare", "--source-bucket", "source/a", "--dest-bucket", "dest/b", "--mode", "md5"},
	} {
		code, out, stderr := runMockCLI(t, args, comparisonUnexpectedRequest(t))
		if code != 2 || stderr == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, stderr)
		}
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 0 {
		t.Fatalf("rejected exports created files: %v", entries)
	}
}

func TestComparisonStatusQueryNormalizesFilter(t *testing.T) {
	calls := 0
	code, out, stderr := runMockCLI(t, []string{"object", "status", "job 1+?", "--after", "9223372036854775806", "--limit", "1000", "--status", " etag_mismatch,DIFFERENT,different"}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/master/comparisons/job 1+?" {
			t.Errorf("bad request: %s %s", r.Method, r.URL.EscapedPath())
		}
		if query.Get("after") != "9223372036854775806" || query.Get("limit") != "1000" || query.Get("status") != "DIFFERENT,ETAG_MISMATCH" {
			t.Errorf("bad query: %s", r.URL.RawQuery)
		}
		adminTestJSON(t, w, comparisonTestPage(map[string]interface{}{"id": "job 1+?", "items": comparisonTestRows()[:2], "nextAfter": 2}))
	})
	if code != 0 || calls != 1 || stderr != "" {
		t.Fatalf("exit %d after %d calls: %s %s", code, calls, out, stderr)
	}
	// An ID that needs shell quoting is quoted in the next-page command.
	next := "下一页：" + comparisonCommand("status", "job 1+?", "--after", "2", "--limit", "1000", "--status", "DIFFERENT,ETAG_MISMATCH") + "\n"
	if !strings.Contains(out, "不一致") || !strings.Contains(out, "ETag 不同") || !strings.Contains(out, next) || !strings.Contains(next, quoteTaskCursor("job 1+?")) {
		t.Fatal(out)
	}
}

func TestComparisonStatusRendersDedicatedSummaryAndRows(t *testing.T) {
	page := comparisonTestPage(map[string]interface{}{"items": comparisonTestRows(), "nextAfter": 4, "checkpoint": "2026/k.jpg", "resumable": true, "status": "INTERRUPTED", "errorDetail": "source LIST: HTTP 503 SlowDown (request ID 9F)", "error": "比对未能完成", "endedAt": nil})
	handler := func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, page) }
	started := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04:05")
	for _, test := range []struct {
		locale       string
		args         []string
		wants, avoid []string
	}{
		{"zh", nil, []string{
			"ID", "job-1", "已中断", "方式", "元数据", "范围", "存储桶/前缀", "src/photos/2026/", "dst/archive/backup/",
			"已检查", "1200", "一致", "不一致", "ETag 不同", "目标缺失", "目标多余", "比对期间已变化", "读取失败", "两端均不存在",
			"已保存差异", "已用时间", "1m0s", "速率", "20.0 个对象/秒", "检查点", "2026/k.jpg", "可继续", "是", "开始时间", started,
			"错误", "比对未能完成", "错误详情", "source LIST: HTTP 503 SlowDown (request ID 9F)",
			"编号", "源 Key", "目标 Key", "源大小", "目标大小", "说明", "2 KiB", "1 KiB",
			"不同：大小、Content-Type", "不同：ETag", "2026/c jpg",
			"Cannot read target metadata（target HEAD: HTTP 403 AccessDenied (request ID 1A2B)）",
			"下一页：object status job-1 --after 4 --limit 50\n", "可用 object resume job-1 从检查点之后继续",
		}, []string{"重试方式", "请求上限范围", "rowsTruncated", "resultLimit", "已读取", "截断原因", "结束时间", "\t"}},
		{"en", []string{"--status", "DIFFERENT,ERROR,ETAG_MISMATCH,MISSING_TARGET", "--limit", "4"}, []string{
			"Status", "Interrupted", "Mode", "Metadata", "Scope", "Bucket/prefix", "Checked", "ETag differs", "Missing from target",
			"Only on target", "Changed during comparison", "Read failed", "Missing on both sides", "Stored differences", "Elapsed",
			"20.0 objects/s", "Checkpoint", "Resumable", "Yes", "No.", "Source key", "Target size", "Details",
			"Differs: size, Content-Type", "Differs: ETag", "Cannot read target metadata (target HEAD: HTTP 403 AccessDenied (request ID 1A2B))",
			"Next page: object status job-1 --after 4 --limit 4 --status DIFFERENT,ETAG_MISMATCH,MISSING_TARGET,ERROR\n",
			"Resume after the checkpoint with object resume job-1",
		}, []string{"Retry mode", "Request limit scope", "方式"}},
	} {
		args := append([]string{"--lang", test.locale, "object", "status", "job-1"}, test.args...)
		code, out, stderr := runMockCLI(t, args, handler)
		if code != 0 || stderr != "" {
			t.Fatalf("%s: exit %d: %s %s", test.locale, code, out, stderr)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s output lacks %q:\n%s", test.locale, want, out)
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out, avoid) {
				t.Errorf("%s output contains %q:\n%s", test.locale, avoid, out)
			}
		}
	}
	// Content mode shows bytes read; an empty page says so.
	content := comparisonTestPage(map[string]interface{}{"mode": "content", "bytesRead": 3 << 20, "rows": 0, "elapsedMillis": 0})
	code, out, _ := runMockCLI(t, []string{"object", "status", "job-1"}, func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, content) })
	if code != 0 || !strings.Contains(out, "内容 SHA-256") || !strings.Contains(out, "已读取") || !strings.Contains(out, "3 MiB") || !strings.Contains(out, "本页没有差异记录") || strings.Contains(out, "下一页") || strings.Contains(out, "个对象/秒") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestComparisonStatusJSONIsTheMasterPage(t *testing.T) {
	page := comparisonTestPage(map[string]interface{}{"items": comparisonTestRows(), "nextAfter": 4})
	code, out, stderr := runMockCLI(t, []string{"--json", "object", "status", "job-1"}, func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, page) })
	var got, want map[string]interface{}
	encoded, _ := json.Marshal(page)
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal(encoded, &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}

func TestComparisonTruncatedRowsAreIncomplete(t *testing.T) {
	for _, test := range []struct {
		name  string
		page  map[string]interface{}
		wants []string
	}{
		{"budget", comparisonTestPage(map[string]interface{}{"rowsTruncated": true, "truncatedReason": "STORAGE_BUDGET", "lastStoredKey": "2026/z.jpg"}), []string{"超过节点的比对结果存储预算", "最后保存的 Key", "2026/z.jpg"}},
		{"disk", comparisonTestPage(map[string]interface{}{"rowsTruncated": true, "truncatedReason": "LOW_DISK_SPACE"}), []string{"节点磁盘剩余空间不足"}},
		{"legacy Master", comparisonTestPage(map[string]interface{}{"rowsTruncated": true, "resultLimit": 10000}), []string{"该节点版本最多保存 1 万条差异"}},
		{"unknown reason", comparisonTestPage(map[string]interface{}{"rowsTruncated": true}), []string{"差异明细未全部保存"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, test.page) }
			code, out, stderr := runMockCLI(t, []string{"object", "status", "job-1"}, handler)
			if code != 3 || !strings.Contains(stderr, "返回不完整") || !strings.Contains(stderr, "差异明细") {
				t.Fatalf("exit %d: %s %s", code, out, stderr)
			}
			for _, want := range append(test.wants, "截断原因") {
				if !strings.Contains(out, want) || !strings.Contains(stderr, test.wants[0]) {
					t.Fatalf("missing %q: %s %s", want, out, stderr)
				}
			}
			code, out, _ = runMockCLI(t, []string{"--json", "object", "status", "job-1"}, handler)
			var decoded map[string]interface{}
			if code != 3 || json.Unmarshal([]byte(out), &decoded) != nil || decoded["rowsTruncated"] != true {
				t.Fatalf("JSON exit %d: %s", code, out)
			}
		})
	}
}

func TestComparisonStatusFilterNeedsAFilteringMaster(t *testing.T) {
	// A Master without filters ignores status= and returns every row.
	code, out, stderr := runMockCLI(t, []string{"object", "status", "job-1", "--status", "DIFFERENT"}, func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, comparisonTestPage(map[string]interface{}{"items": comparisonTestRows()}))
	})
	if code != 1 || !strings.Contains(stderr, "当前 Master 不支持按结果筛选，请先升级 Master") || strings.Contains(out, "2026/a.jpg") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
}

func TestComparisonListShowsHistoryAndUnreachableNodes(t *testing.T) {
	list := map[string]interface{}{
		"complete": false,
		"items": []interface{}{
			comparisonTestPage(map[string]interface{}{"id": "job-2", "status": "RUNNING", "mode": "content", "compared": 10, "matched": 7, "nodeId": "n1", "nodeName": "Worker 1", "rowsTruncated": true, "items": nil}),
			map[string]interface{}{"id": "job-1", "status": "UNAVAILABLE", "mode": "metadata", "scope": "object", "source": map[string]interface{}{"kind": "S3", "ref": "src", "bucket": "photos"}, "destination": map[string]interface{}{"kind": "S3", "ref": "dst", "bucket": "archive"}, "sourcePrefix": "a.jpg", "destinationPrefix": "b.jpg", "startedAt": "2026-11-08T10:00:00Z", "nodeId": "n2", "nodeName": "Worker 2", "compared": 0, "matched": 0},
		},
		"unavailableNodes": []interface{}{map[string]interface{}{"nodeId": "n2", "nodeName": "Worker 2", "count": 1}},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/master/comparisons" {
			t.Errorf("bad request: %s %s", r.Method, r.URL)
		}
		adminTestJSON(t, w, list)
	}
	code, out, stderr := runMockCLI(t, []string{"object", "list"}, handler)
	for _, want := range []string{"ID", "状态", "方式", "范围", "源", "目标", "已检查", "差异", "开始时间", "节点", "job-2", "正在比对", "内容 SHA-256", "src/photos/2026/", "Worker 1", "job-1", "节点不可用", "单个对象", "src/photos/a.jpg", "dst/archive/b.jpg", "Worker 2", "共 2 个比对", "节点 Worker 2 暂时无法连接，1 个比对的最新状态未知"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	if code != 3 || !strings.Contains(stderr, "返回不完整") || !strings.Contains(lines[1], " 10 ") || !strings.Contains(lines[1], " 3 ") || !strings.Contains(lines[2], "—") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	// A listed comparison's truncated rows do not make the list incomplete.
	list["complete"], list["unavailableNodes"], list["items"] = true, []interface{}{}, list["items"].([]interface{})[:1]
	code, out, stderr = runMockCLI(t, []string{"--lang", "en", "object", "list"}, handler)
	if code != 0 || stderr != "" || !strings.Contains(out, "1 comparison\n") || !strings.Contains(out, "Differences") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	list["items"] = []interface{}{}
	code, out, _ = runMockCLI(t, []string{"object", "list"}, handler)
	if code != 0 || !strings.Contains(out, "暂无比对记录") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestComparisonRoutesMissingOnOlderMastersAskForUpdate(t *testing.T) {
	unknown := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "Unknown API endpoint"})
	}
	code, _, stderr := runMockCLI(t, []string{"object", "list"}, func(w http.ResponseWriter, r *http.Request) { unknown(w) })
	if code != 1 || !strings.Contains(stderr, "当前 Master 不支持列出比对，请先升级 Master") {
		t.Fatalf("%d %s", code, stderr)
	}
	for _, test := range []struct {
		exists bool
		want   string
	}{{true, "当前 Master 不支持继续比对，请先升级 Master"}, {false, "Comparison not found"}} {
		code, _, stderr := runMockCLI(t, []string{"object", "resume", "job-1"}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/master/comparisons/job-1/resume" && test.exists:
				unknown(w)
			case r.Method == http.MethodGet && r.URL.Path == "/api/master/comparisons/job-1" && test.exists:
				adminTestJSON(t, w, comparisonTestPage(nil))
			default:
				w.WriteHeader(http.StatusNotFound)
				adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "Comparison not found"})
			}
		})
		if code != 1 || !strings.Contains(stderr, test.want) {
			t.Fatalf("%v: %d %s", test.exists, code, stderr)
		}
	}
}

func TestComparisonActionsPrintDedicatedMessages(t *testing.T) {
	for _, test := range []struct {
		args          []string
		method, path  string
		status        int
		response      map[string]interface{}
		code          int
		wants, avoids []string
	}{
		{[]string{"object", "compare", "--source-bucket", "src/photos", "--dest-bucket", "dst/archive", "--source-prefix", "2026/", "--dest-prefix", "backup/"}, http.MethodPost, "/api/master/comparisons", http.StatusAccepted,
			comparisonTestPage(map[string]interface{}{"id": "-Ab_9", "status": "RUNNING"}), 0, []string{"比对已开始，ID：-Ab_9\n", "查看进度：object status --watch 5 -- -Ab_9\n"}, []string{"重试方式", "请求上限范围"}},
		{[]string{"object", "compare", "--source-bucket", "src/photos", "--dest-bucket", "dst/archive", "--source-prefix", "2026/", "--dest-prefix", "backup/"}, http.MethodPost, "/api/master/comparisons", http.StatusBadGateway,
			map[string]interface{}{"success": false, "id": "job-9", "message": "Agent did not answer"}, 1, []string{"启动结果未确认，比对 ID：job-9；稍后用 object status job-9 查看"}, nil},
		{[]string{"object", "resume", "job-1"}, http.MethodPost, "/api/master/comparisons/job-1/resume", http.StatusAccepted,
			comparisonTestPage(map[string]interface{}{"status": "RUNNING", "resumes": 1}), 0, []string{"已继续比对 job-1\n", "查看进度：object status job-1 --watch 5\n"}, nil},
		{[]string{"object", "resume", "job-1"}, http.MethodPost, "/api/master/comparisons/job-1/resume", http.StatusConflict,
			map[string]interface{}{"success": false, "message": "Another comparison is running on this node"}, 1, nil, nil},
		{[]string{"object", "cancel", "job-1"}, http.MethodPost, "/api/master/comparisons/job-1/cancel", http.StatusOK,
			comparisonTestPage(map[string]interface{}{"status": "CANCELING"}), 0, []string{"已请求取消比对 job-1\n"}, []string{"重试方式"}},
		{[]string{"object", "cancel", "job-1"}, http.MethodPost, "/api/master/comparisons/job-1/cancel", http.StatusOK,
			comparisonTestPage(nil), 0, []string{"比对 job-1 当前状态：已完成\n"}, nil},
		{[]string{"object", "delete", "--", "-job"}, http.MethodDelete, "/api/master/comparisons/-job", http.StatusOK,
			map[string]interface{}{"success": true}, 0, []string{"已删除比对 -job 的结果\n"}, []string{"成功"}},
		{[]string{"object", "delete", "job-1"}, http.MethodDelete, "/api/master/comparisons/job-1", http.StatusConflict,
			map[string]interface{}{"success": false, "message": "Comparison is running; cancel it before deleting"}, 1, nil, nil},
	} {
		code, out, stderr := runMockCLI(t, test.args, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != test.method || r.URL.Path != test.path {
				t.Errorf("%v: bad request %s %s", test.args, r.Method, r.URL)
			}
			w.WriteHeader(test.status)
			adminTestJSON(t, w, test.response)
		})
		if code != test.code {
			t.Errorf("%v: exit %d: %s %s", test.args, code, out, stderr)
		}
		if message := getString(test.response, "message"); test.code != 0 && !strings.Contains(stderr, message) {
			t.Errorf("%v: stderr lacks %q: %s", test.args, message, stderr)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%v lacks %q: %s", test.args, want, out)
			}
		}
		for _, avoid := range test.avoids {
			if strings.Contains(out, avoid) {
				t.Errorf("%v contains %q: %s", test.args, avoid, out)
			}
		}
	}
	// --json keeps the Master's reply to a start.
	progress := comparisonTestPage(map[string]interface{}{"status": "RUNNING", "items": nil})
	code, out, _ := runMockCLI(t, []string{"--json", "object", "compare", "--source-bucket", "src/photos", "--dest-bucket", "dst/archive", "--mode", "content"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		adminTestJSON(t, w, progress)
	})
	var got, want map[string]interface{}
	encoded, _ := json.Marshal(progress)
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal(encoded, &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestComparisonRepliesThatAreNotComparisonsAreRejected(t *testing.T) {
	for _, test := range []struct {
		args  []string
		reply interface{}
		want  string
	}{
		{[]string{"object", "status", "job-1"}, map[string]interface{}{"success": true, "data": map[string]interface{}{}}, "服务器未返回有效的比对结果"},
		{[]string{"object", "status", "job-1"}, map[string]interface{}{"id": "job-1", "status": "RUNNING"}, "服务器未返回有效的比对结果"},
		{[]string{"object", "list"}, map[string]interface{}{"success": true, "data": map[string]interface{}{}}, "服务器未返回有效的比对列表"},
		{[]string{"object", "list"}, map[string]interface{}{"items": map[string]interface{}{}, "complete": true}, "服务器未返回有效的比对列表"},
		{[]string{"object", "list"}, map[string]interface{}{"items": []interface{}{"job-1"}, "complete": true}, "服务器未返回有效的比对列表"},
	} {
		code, out, stderr := runMockCLI(t, test.args, func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, test.reply) })
		if code != 1 || !strings.Contains(stderr, test.want) || out != "" {
			t.Errorf("%v %v: exit %d: %q %q", test.args, test.reply, code, out, stderr)
		}
	}
	// "items": null is an empty list.
	code, out, stderr := runMockCLI(t, []string{"object", "list"}, func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, map[string]interface{}{"items": nil, "complete": true, "unavailableNodes": nil})
	})
	if code != 0 || !strings.Contains(out, "暂无比对记录") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
}

func TestComparisonActionsOnTruncatedResultsSucceed(t *testing.T) {
	// Starting, resuming or canceling succeeds even when the comparison's stored
	// rows are truncated; only a status page is an incomplete answer.
	truncated := comparisonTestPage(map[string]interface{}{"status": "RUNNING", "rowsTruncated": true, "truncatedReason": "STORAGE_BUDGET", "items": nil})
	for _, args := range [][]string{
		{"object", "resume", "job-1"},
		{"object", "cancel", "job-1"},
		{"object", "compare", "--source-bucket", "src/photos", "--dest-bucket", "dst/archive", "--mode", "content"},
	} {
		for _, mode := range [][]string{nil, {"--json"}} {
			code, out, stderr := runMockCLI(t, append(mode, args...), func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, truncated) })
			if code != 0 || stderr != "" {
				t.Errorf("%v %v: exit %d: %s %s", mode, args, code, out, stderr)
			}
		}
	}
}

func TestComparisonStatusAndListAreWatchable(t *testing.T) {
	for _, args := range [][]string{{"object", "status", "job-1"}, {"object", "list"}} {
		if !watchable(args) {
			t.Errorf("%v is not watchable", args)
		}
	}
	for _, args := range [][]string{{"object", "export", "job-1"}, {"object", "resume", "job-1"}, {"object", "compare"}, {"object", "delete", "job-1"}} {
		if watchable(args) {
			t.Errorf("%v is watchable", args)
		}
	}
}

func TestComparisonCommandHintsQuoteIDs(t *testing.T) {
	for _, test := range []struct{ id, want string }{
		{"Abc_-9", "object status Abc_-9 --watch 5"},
		{"-Abc", "object status --watch 5 -- -Abc"},
		{"--lang", "object status --watch 5 -- --lang"},
	} {
		if got := comparisonCommand("status", test.id, "--watch", "5"); got != test.want {
			t.Errorf("%q: %q", test.id, got)
		}
	}
	if got := comparisonCommand("resume", "a b"); got != "object resume "+quoteTaskCursor("a b") {
		t.Fatal(got)
	}
}

func TestComparisonExportCompletenessFollowsSummaryRules(t *testing.T) {
	for _, test := range []struct {
		progress map[string]interface{}
		exported int64
		filtered bool
		want     []string
	}{
		{map[string]interface{}{"status": "COMPLETED", "compared": 10.0, "matched": 7.0, "rows": 3.0}, 3, false, []string{}},
		{map[string]interface{}{"status": "COMPLETED", "compared": 10.0, "matched": 7.0, "rows": 3.0}, 1, true, []string{}},
		{map[string]interface{}{"status": "COMPLETED", "compared": 10.0, "matched": 7.0, "rows": 3.0}, 2, false, []string{"ROW_COUNT_MISMATCH"}},
		{map[string]interface{}{"status": "RUNNING", "compared": 10.0, "matched": 7.0, "rows": 3.0}, 3, false, []string{"NOT_COMPLETED:RUNNING"}},
		{map[string]interface{}{"status": "COMPLETED", "compared": 20000.0, "matched": 0.0, "rows": 10000.0, "rowsTruncated": true, "resultLimit": 10000.0}, 10000, false, []string{"ROWS_TRUNCATED:LEGACY_LIMIT", "ROW_COUNT_MISMATCH"}},
	} {
		complete, reasons := comparisonExportCompleteness(test.progress, test.exported, test.filtered)
		if complete != (len(test.want) == 0) || !reflect.DeepEqual(reasons, test.want) {
			t.Errorf("%v: %v %v", test.progress, complete, reasons)
		}
	}
}

func TestComparisonMessagesHaveTranslations(t *testing.T) {
	keys := []string{}
	for _, text := range []downloadText{reportDownload.text, comparisonExportText} {
		value := reflect.ValueOf(text)
		for i := 0; i < value.NumField(); i++ {
			keys = append(keys, value.Field(i).String())
		}
	}
	for _, labels := range []map[string]string{comparisonStatusLabels, comparisonReasonLabels} {
		for _, label := range labels {
			keys = append(keys, label)
		}
	}
	for _, key := range keys {
		if _, ok := cliMessages[key]; key != "" && !ok && strings.ContainsFunc(key, func(r rune) bool { return r > 0x2e80 }) {
			t.Errorf("missing translation for %q", key)
		}
	}
}

// comparisonTestArchive builds an export ZIP; summary overrides replace
// computed fields and nil removes them.
func comparisonTestArchive(t *testing.T, rows, retry []string, summary map[string]interface{}, omit string) []byte {
	t.Helper()
	values := map[string]interface{}{
		"comparisonId": "job-1", "status": "COMPLETED", "complete": true, "incompleteReasons": []interface{}{},
		"counts":     map[string]interface{}{"compared": 10, "matched": 10 - len(rows), "different": len(rows), "etagMismatch": 0, "missing": 0, "extra": 0, "changed": 0, "errors": 0, "bothMissing": 0},
		"storedRows": len(rows), "exportedRows": len(rows), "retryKeys": len(retry), "retryKeysOmitted": 0, "retryPrefixesMatch": true,
	}
	for key, value := range summary {
		if value == nil {
			delete(values, key)
		} else {
			values[key] = value
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var csvText bytes.Buffer
	records := csv.NewWriter(&csvText)
	records.Write(strings.Split("number,key,status,sourceKey,destinationKey,sourceSize,destinationSize,sourceSha256,destinationSha256,reasons,error,errorDetail", ","))
	for i, row := range rows {
		records.Write([]string{fmt.Sprint(i + 1), row, "DIFFERENT", "2026/" + row, "backup/" + row, "8", "8", "", "", "sha256", "", ""})
	}
	records.Flush()
	retryText := ""
	for _, key := range retry {
		retryText += key + "\n"
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range [][2]string{{"differences.csv", csvText.String()}, {"retry-source-keys.incr", retryText}, {"summary.json", string(encoded)}} {
		if entry[0] == omit {
			continue
		}
		// Stored entries keep their text visible, so tests can corrupt it.
		file, err := writer.CreateHeader(&zip.FileHeader{Name: entry[0], Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// runComparisonCLI runs the CLI against a real HTTP server, so exports stream.
func runComparisonCLI(t *testing.T, server *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	var out, stderr bytes.Buffer
	code := runCLI(append([]string{"--host", server.URL}, args...), strings.NewReader(""), &out, &stderr)
	return code, out.String(), stderr.String()
}

// streamComparisonExport writes body in small flushed chunks, aborting the
// connection after abortAfter bytes when it is positive.
func streamComparisonExport(w http.ResponseWriter, contentType string, body []byte, abortAfter int) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	for start := 0; start < len(body); start += 100 {
		end := min(start+100, len(body))
		if abortAfter > 0 && end > abortAfter {
			panic(http.ErrAbortHandler)
		}
		w.Write(body[start:end])
		w.(http.Flusher).Flush()
	}
}

func comparisonDirectoryEntries(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestComparisonExportZIPVerifiesAndSummarizes(t *testing.T) {
	rows, retry := []string{"a.jpg", "b,\"c\".jpg", "=d.jpg"}, []string{"2026/a.jpg", "2026/b,\"c\".jpg"}
	archive := comparisonTestArchive(t, rows, retry, nil, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/master/comparisons/job-1/export" || r.URL.Query().Get("format") != "zip" || r.Header.Get("Accept") != "application/zip" {
			t.Errorf("bad export request: %s %s %s", r.Method, r.URL, r.Header.Get("Accept"))
		}
		if status := r.URL.Query().Get("status"); status != "" && status != "DIFFERENT,MISSING_TARGET" {
			t.Errorf("bad filter: %s", status)
		}
		streamComparisonExport(w, "application/zip", archive, 0)
	}))
	defer server.Close()
	directory := t.TempDir()
	output := filepath.Join(directory, "export.zip")
	code, out, stderr := runComparisonCLI(t, server, "object", "export", "job-1", "--output", output, "--status", "missing_target,different")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	for _, want := range []string{"保存位置", output, "格式", "ZIP", "筛选", "不一致、目标缺失", "已检查", "10", "已导出差异", "3", "重新迁移的源 Key", "2", "完整", "是"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if saved, err := os.ReadFile(output); err != nil || !bytes.Equal(saved, archive) {
		t.Fatalf("saved archive differs: %v", err)
	}
	// The same destination is never replaced.
	code, _, stderr = runComparisonCLI(t, server, "object", "export", "job-1", "--output", output)
	if code != 1 || !strings.Contains(stderr, "目标文件已存在") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	jsonOutput := filepath.Join(directory, "json.zip")
	code, out, _ = runComparisonCLI(t, server, "--json", "object", "export", "job-1", "--output", jsonOutput)
	var result map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &result) != nil {
		t.Fatalf("exit %d: %s", code, out)
	}
	summary, _ := result["summary"].(map[string]interface{})
	if result["complete"] != true || result["exportedRows"] != 3.0 || result["retryKeys"] != 2.0 || result["format"] != "zip" || result["output"] != jsonOutput || summary["comparisonId"] != "job-1" || summary["storedRows"] != 3.0 {
		t.Fatalf("bad JSON result: %s", out)
	}
	if names := comparisonDirectoryEntries(t, directory); !reflect.DeepEqual(names, []string{"export.zip", "json.zip"}) {
		t.Fatalf("unexpected files: %v", names)
	}
}

func TestComparisonExportZIPReportsIncompleteExports(t *testing.T) {
	archive := comparisonTestArchive(t, []string{"a.jpg"}, []string{"2026/a.jpg"}, map[string]interface{}{
		"status": "RUNNING", "complete": false, "incompleteReasons": []interface{}{"NOT_COMPLETED:RUNNING", "ROWS_TRUNCATED:STORAGE_BUDGET", "ROW_COUNT_MISMATCH"},
		"truncatedReason": "STORAGE_BUDGET", "lastStoredKey": "a.jpg", "retryKeysOmitted": 2, "retryPrefixesMatch": false,
	}, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamComparisonExport(w, "application/zip", archive, 0)
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "export.zip")
	code, out, stderr := runComparisonCLI(t, server, "object", "export", "job-1", "--output", output)
	for _, want := range []string{"完整", "否", "不完整原因", "比对未完成（正在比对）", "差异明细未全部保存（超过节点的比对结果存储预算）", "差异数与统计不一致", "截断原因", "最后保存的 Key", "2 个源 Key 无法写入 retry-source-keys.incr", "源前缀与目标前缀不同"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code != 3 || !strings.Contains(stderr, "返回不完整") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal("an incomplete but valid export must still be saved")
	}
}

func TestComparisonExportZIPRejectsInvalidArchives(t *testing.T) {
	rows, retry := []string{"a.jpg", "b.jpg"}, []string{"2026/a.jpg"}
	valid := comparisonTestArchive(t, rows, retry, nil, "")
	corrupt := append([]byte(nil), valid...)
	corrupt[bytes.Index(corrupt, []byte("2026/a.jpg"))] = 'X'
	for _, test := range []struct {
		name        string
		contentType string
		body        []byte
		abortAfter  int
		want        string
	}{
		{"row count", "application/zip", comparisonTestArchive(t, rows, retry, map[string]interface{}{"exportedRows": 3}, ""), 0, "导出文件不一致"},
		{"retry count", "application/zip", comparisonTestArchive(t, rows, retry, map[string]interface{}{"retryKeys": 0}, ""), 0, "导出文件不一致"},
		{"no counts", "application/zip", comparisonTestArchive(t, rows, retry, map[string]interface{}{"exportedRows": nil}, ""), 0, "summary.json 缺少导出统计"},
		{"no summary", "application/zip", comparisonTestArchive(t, rows, retry, nil, "summary.json"), 0, "导出文件缺少 summary.json"},
		{"no retry list", "application/zip", comparisonTestArchive(t, rows, retry, nil, "retry-source-keys.incr"), 0, "导出文件缺少 retry-source-keys.incr"},
		{"checksum", "application/zip", corrupt, 0, "ZIP 文件校验失败"},
		{"aborted", "application/zip", valid, len(valid) / 2, "导出文件下载中断"},
		{"not a zip", "application/zip", []byte("not a zip"), 0, "下载内容不是完整的 ZIP 文件"},
		{"login page", "text/html", []byte("<html>login</html>"), 0, "服务器未返回比对导出文件"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				streamComparisonExport(w, test.contentType, test.body, test.abortAfter)
			}))
			defer server.Close()
			directory := t.TempDir()
			code, out, stderr := runComparisonCLI(t, server, "object", "export", "job-1", "--output", filepath.Join(directory, "export.zip"))
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Fatalf("exit %d: %s %s", code, out, stderr)
			}
			if names := comparisonDirectoryEntries(t, directory); len(names) != 0 {
				t.Fatalf("rejected export left files: %v", names)
			}
		})
	}
}

func TestComparisonExportOnOlderMasterAsksForUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/master/comparisons/job-1" {
			adminTestJSON(t, w, comparisonTestPage(nil))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "Unknown API endpoint"})
	}))
	defer server.Close()
	for _, format := range []string{"zip", "jsonl"} {
		directory := t.TempDir()
		code, _, stderr := runComparisonCLI(t, server, "object", "export", "job-1", "--format", format, "--output", filepath.Join(directory, "export"))
		if code != 1 || !strings.Contains(stderr, "当前 Master 不支持导出比对结果，请先升级 Master") {
			t.Fatalf("%s: exit %d: %s", format, code, stderr)
		}
		if names := comparisonDirectoryEntries(t, directory); len(names) != 0 {
			t.Fatalf("failed export left files: %v", names)
		}
	}
}

func comparisonJSONLServer(t *testing.T, progress map[string]interface{}, contentType, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/comparisons/job-1":
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("progress read pages rows: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			adminTestJSON(t, w, progress)
		case "/api/master/comparisons/job-1/export":
			if r.URL.Query().Get("format") != "jsonl" || r.Header.Get("Accept") != "application/x-ndjson" {
				t.Errorf("bad export request: %s %s", r.URL.RawQuery, r.Header.Get("Accept"))
			}
			streamComparisonExport(w, contentType, []byte(body), 0)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestComparisonExportJSONL(t *testing.T) {
	row := func(number int, status string) string {
		return fmt.Sprintf(`{"number":%d,"key":"k%d","sourceKey":"2026/k%d","destinationKey":"backup/k%d","status":%q,"sourceSize":1,"destinationSize":2}`+"\n", number, number, number, number, status)
	}
	completed := comparisonTestPage(map[string]interface{}{"compared": 10, "matched": 7, "rows": 3})
	three := row(1, "DIFFERENT") + row(5, "MISSING_TARGET") + row(9, "DIFFERENT")
	for _, test := range []struct {
		name        string
		progress    map[string]interface{}
		contentType string
		body        string
		args        []string
		code        int
		rows        float64
		reasons     []interface{}
		failure     string
	}{
		{"complete", completed, "application/x-ndjson; charset=utf-8", three, nil, 0, 3, []interface{}{}, ""},
		{"filtered", completed, "application/x-ndjson", row(5, "MISSING_TARGET"), []string{"--status", "MISSING_TARGET"}, 0, 1, []interface{}{}, ""},
		{"empty filter", completed, "application/x-ndjson", "", []string{"--status", "BOTH_MISSING"}, 0, 0, []interface{}{}, ""},
		{"running", comparisonTestPage(map[string]interface{}{"status": "RUNNING", "compared": 10, "matched": 7, "rows": 3}), "application/x-ndjson", three, nil, 3, 3, []interface{}{"NOT_COMPLETED:RUNNING"}, ""},
		{"rows missing", completed, "application/x-ndjson", row(1, "DIFFERENT"), nil, 3, 1, []interface{}{"ROW_COUNT_MISMATCH"}, ""},
		{"cut line", completed, "application/x-ndjson", strings.TrimSuffix(three, "\n"), nil, 1, 0, nil, "JSONL 导出不完整：最后一行缺少换行"},
		{"order", completed, "application/x-ndjson", row(5, "DIFFERENT") + row(1, "DIFFERENT"), nil, 1, 0, nil, "JSONL 导出第 2 行无效"},
		{"duplicate", completed, "application/x-ndjson", row(1, "DIFFERENT") + row(1, "DIFFERENT"), nil, 1, 0, nil, "JSONL 导出第 2 行无效"},
		{"unfiltered row", completed, "application/x-ndjson", row(1, "DIFFERENT"), []string{"--status", "ERROR"}, 1, 0, nil, "JSONL 导出第 1 行无效"},
		{"not a row", completed, "application/x-ndjson", "null\n", nil, 1, 0, nil, "JSONL 导出第 1 行无效"},
		{"long line", completed, "application/x-ndjson", `{"number":1,"status":"DIFFERENT","key":"` + strings.Repeat("x", comparisonMaxJSONLLine) + "\"}\n", nil, 1, 0, nil, "JSONL 导出第 1 行无效"},
		{"json reply", completed, "application/json", `{"success":true}` + "\n", nil, 1, 0, nil, "服务器未返回比对导出文件（Content-Type: application/json）"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := comparisonJSONLServer(t, test.progress, test.contentType, test.body)
			directory := t.TempDir()
			output := filepath.Join(directory, "rows.jsonl")
			args := append([]string{"--json", "object", "export", "job-1", "--format", "JSONL", "--output", output}, test.args...)
			code, out, stderr := runComparisonCLI(t, server, args...)
			if code != test.code {
				t.Fatalf("exit %d: %s %s", code, out, stderr)
			}
			if test.failure != "" {
				if !strings.Contains(stderr, test.failure) {
					t.Fatalf("stderr lacks %q: %s", test.failure, stderr)
				}
				if names := comparisonDirectoryEntries(t, directory); len(names) != 0 {
					t.Fatalf("rejected export left files: %v", names)
				}
				return
			}
			var result map[string]interface{}
			if json.Unmarshal([]byte(out), &result) != nil || result["exportedRows"] != test.rows || result["complete"] != (test.code == 0) || !reflect.DeepEqual(result["incompleteReasons"], test.reasons) || result["format"] != "jsonl" {
				t.Fatalf("bad result: %s", out)
			}
			if saved, err := os.ReadFile(output); err != nil || string(saved) != test.body {
				t.Fatalf("saved rows differ: %v %q", err, saved)
			}
		})
	}
}

func TestComparisonExportStopsWhenTheStreamStalls(t *testing.T) {
	original := comparisonExportIdle
	comparisonExportIdle = 20 * time.Millisecond
	t.Cleanup(func() { comparisonExportIdle = original })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !strings.HasSuffix(r.URL.Path, "/export"):
			// The JSONL export reads the comparison first.
			w.Header().Set("Content-Type", "application/json")
			adminTestJSON(t, w, comparisonTestPage(nil))
			return
		case r.URL.Query().Get("format") == "jsonl":
			// One row arrives, then nothing.
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Write([]byte(`{"number":1,"status":"DIFFERENT"}` + "\n"))
			w.(http.Flusher).Flush()
		}
		// A ZIP export never even sends headers.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	for _, format := range []string{"zip", "jsonl"} {
		directory := t.TempDir()
		args := []string{"--host", server.URL, "object", "export", "job-1", "--format", format, "--output", filepath.Join(directory, "export")}
		done := make(chan struct{})
		var code int
		var out, stderr bytes.Buffer
		go func() {
			defer close(done)
			code = runCLI(args, strings.NewReader(""), &out, &stderr)
		}()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("a stalled export did not stop")
		}
		if code != 1 || !strings.Contains(stderr.String(), "导出文件下载中断：20ms 内未收到数据") {
			t.Fatalf("%s: exit %d: %s %s", format, code, out.String(), stderr.String())
		}
		if names := comparisonDirectoryEntries(t, directory); len(names) != 0 {
			t.Fatalf("stalled export left files: %v", names)
		}
	}
}

func TestCountReaderLines(t *testing.T) {
	for text, want := range map[string]int64{"": 0, "a\n": 1, "a": 1, "a\nb": 2, "a\nb\n": 2, "\n\n": 2} {
		if got, err := countReaderLines(strings.NewReader(text)); err != nil || got != want {
			t.Errorf("%q: %d %v", text, got, err)
		}
	}
}
