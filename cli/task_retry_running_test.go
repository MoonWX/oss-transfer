package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// task retry on a RUNNING task asks the Master to retry the failed objects of
// its finished subtasks in place, with the same generation guard as other
// retries; a Master without that support is reported, not hidden.
func TestTaskRetryOnRunningTaskRetriesFailedObjectsInPlace(t *testing.T) {
	for _, upgraded := range []bool{true, false} {
		posts := 0
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				row := taskTestRow(107, "OSS_TO_OSS", "RUNNING")
				row["generation"] = 3
				adminTestJSON(t, w, map[string]interface{}{"data": row})
				return
			}
			posts++
			body := adminTestBody(t, r)
			if r.URL.Path != "/api/master/retry" || !reflect.DeepEqual(body["taskIds"], []interface{}{float64(107)}) || !reflect.DeepEqual(body["expectedGenerations"], map[string]interface{}{"107": float64(3)}) {
				t.Fatal("running retry was not guarded", r.URL, body)
			}
			detail := map[string]interface{}{"taskId": 107, "status": "RUNNING", "generation": 3, "success": true, "accepted": upgraded, "ignored": !upgraded}
			accepted, ignored := 1, 0
			if upgraded {
				detail["mode"], detail["retriedChildren"], detail["retriedObjects"] = "FAILED_OBJECTS", 1, 1
			} else {
				accepted, ignored = 0, 1
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": accepted, "ignored": ignored, "failed": 0, "details": []interface{}{detail}})
		}
		code, out, errout := runMockCLI(t, []string{"--lang", "en", "task", "retry", "107"}, handler)
		if code != 0 || posts != 1 {
			t.Fatal(upgraded, code, posts, out, errout)
		}
		if upgraded && (!strings.Contains(out, "#107") || !strings.Contains(out, "Retry started for 1 failed object (1 child task); the task keeps running") || strings.Contains(out, "again")) {
			t.Fatalf("in-place retry result not shown:\n%s", out)
		}
		if !upgraded && !strings.Contains(out, "upgrade the Master to retry its failed objects") {
			t.Fatalf("old Master was not reported:\n%s", out)
		}
	}
	// Retrying every failed task still selects only ended tasks.
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(2, "OSS_TO_OSS", "RUNNING")}, "hasMore": false})
			return
		}
		t.Fatal("retry --all included a running task", r.URL)
	})
	if result, err := taskControl(client, "retry", []string{"--all", "--yes"}); err != nil || getInt(result, "accepted") != 0 {
		t.Fatal(result, err)
	}
}

// While a large OSS→OSS task is still listing objects, the Master reports it
// as RUNNING with creationStatus RUNNING. Retry still retries the failed
// objects of its finished subtasks in place, and resume says it is already
// running instead of refusing the task.
func TestTaskRetryAndResumeWhileTheScanIsStillListing(t *testing.T) {
	scanning := func() map[string]interface{} {
		return map[string]interface{}{"taskIndex": 107, "direction": "OSS_TO_OSS", "status": "RUNNING", "generation": 3, "ossTaskGroup": true, "creationStatus": "RUNNING", "scanComplete": false}
	}
	posts := []string{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"data": scanning()})
			return
		}
		posts = append(posts, r.URL.Path)
		detail := map[string]interface{}{"taskId": 107, "status": "RUNNING", "generation": 3, "success": true, "accepted": true, "ignored": false, "mode": "FAILED_OBJECTS", "retriedChildren": 2, "retriedObjects": 5}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "ignored": 0, "failed": 0, "details": []interface{}{detail}})
	}
	code, out, errout := runMockCLI(t, []string{"--lang", "en", "task", "retry", "107"}, handler)
	if code != 0 || !reflect.DeepEqual(posts, []string{"/api/master/retry"}) || !strings.Contains(out, "#107") || strings.Contains(errout, "not allowed") {
		t.Fatalf("retry of a listing task was refused: %d %v\n%s%s", code, posts, out, errout)
	}
	posts = nil
	code, out, errout = runMockCLI(t, []string{"--lang", "en", "task", "resume", "107"}, handler)
	if code != 0 || len(posts) != 0 || !strings.Contains(out, "No action needed (current state: Running)") {
		t.Fatalf("resume of a listing task: %d %v\n%s%s", code, posts, out, errout)
	}
}

// One in-place retry reopens at most 64 subtasks. When the Master says that
// more subtasks were left, the CLI says how many it reopened and asks for
// another run instead of implying that every failed object is retrying.
func TestTaskRetryOnRunningTaskSaysWhenSubtasksAreLeft(t *testing.T) {
	for _, more := range []bool{true, false} {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(107, "OSS_TO_OSS", "RUNNING")})
				return
			}
			detail := map[string]interface{}{"taskId": 107, "status": "RUNNING", "generation": 1, "success": true, "accepted": true, "ignored": false, "mode": "FAILED_OBJECTS", "retriedChildren": 64, "retriedObjects": 1280, "hasMore": more}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "ignored": 0, "failed": 0, "details": []interface{}{detail}})
		}
		for _, test := range []struct{ lang, retried, again string }{
			{"zh", "#107  已开始重试 1280 个失败对象（64 个子任务），任务继续运行", "；其他子任务可能还有未重试的失败对象，请再次运行 oss-transfer-cli task retry 107"},
			{"en", "#107  Retry started for 1280 failed objects (64 child tasks); the task keeps running", "; other child tasks may still have failed objects that were not retried; run oss-transfer-cli task retry 107 again"},
			{"ja", "#107  失敗した 1280 件のオブジェクトの再試行を開始しました（子タスク 64 件）。タスクは実行を続けます", "oss-transfer-cli task retry 107 を再度実行してください"},
		} {
			code, out, errout := runMockCLI(t, []string{"--lang", test.lang, "task", "retry", "107"}, handler)
			if code != 0 || !strings.Contains(out, test.retried) || strings.Contains(out, test.again) != more {
				t.Fatalf("%s hasMore=%v: %d\n%s%s", test.lang, more, code, out, errout)
			}
		}
	}
}

// The Master accepts a resume or retry and sets the task RESUMING; it runs
// again only after every execution node is ready. The CLI reports the request
// and the current state, and says where to follow it.
func TestTaskResumeAndRetryReportAcceptanceNotCompletion(t *testing.T) {
	for _, action := range []string{"resume", "retry"} {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(7, "OSS_TO_OSS", "FAILED")})
				return
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "failed": 0, "ignored": 0, "details": []interface{}{map[string]interface{}{"taskId": 7, "status": "RESUMING", "generation": 2, "success": true, "accepted": true, "ignored": false}}})
		}
		for _, test := range []struct{ lang, outcome, follow, avoid string }{
			{"zh", "#7  已请求" + map[string]string{"resume": "继续", "retry": "重试"}[action] + "（当前状态：恢复中）", "可用 oss-transfer-cli task show 7 查看进展和等待原因", "已恢复执行"},
			{"en", "#7  " + map[string]string{"resume": "Resume", "retry": "Retry"}[action] + " requested (current state: Resuming)", "check its progress and what it waits for with oss-transfer-cli task show 7", "Resumed"},
			{"ja", "#7  " + map[string]string{"resume": "再開", "retry": "再試行"}[action] + "を要求しました（現在の状態: 再開中）", "oss-transfer-cli task show 7 で確認してください", "再開しました"},
		} {
			code, out, errout := runMockCLI(t, []string{"--lang", test.lang, "task", action, "7"}, handler)
			if code != 0 || !strings.Contains(out, test.outcome) || !strings.Contains(out, test.follow) || strings.Contains(out, test.avoid) {
				t.Fatalf("%s %s: %d\n%s%s", action, test.lang, code, out, errout)
			}
		}
	}
	// A task that is already running again needs no follow-up hint.
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(7, "LOCAL_TO_OSS", "STOPPED")})
			return
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"success": true, "accepted": 1, "failed": 0, "ignored": 0, "details": []interface{}{map[string]interface{}{"taskId": 7, "status": "RUNNING", "generation": 2, "success": true, "accepted": true, "ignored": false}}}})
	}
	code, out, errout := runMockCLI(t, []string{"task", "resume", "7"}, handler)
	if code != 0 || !strings.Contains(out, "#7  已请求继续（当前状态：执行中）") || strings.Contains(out, "task show") {
		t.Fatalf("%d\n%s%s", code, out, errout)
	}
}
