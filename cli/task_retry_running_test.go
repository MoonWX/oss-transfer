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
		if upgraded && (!strings.Contains(out, "Retried objects: 1") || !strings.Contains(out, "Retried subtasks: 1") || !strings.Contains(out, "Accepted: 1")) {
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
