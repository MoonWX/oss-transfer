package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestTaskListAllFollowsCursorsAcrossEmptyFilteredPages(t *testing.T) {
	calls := []string{}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/status" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("includeSubtasks") != "false" {
			t.Fatalf("unexpected list %s", r.URL)
		}
		before := r.URL.Query().Get("beforeId")
		calls = append(calls, before)
		result := map[string]interface{}{"tasks": []interface{}{}, "partial": false, "hasMore": true}
		switch before {
		case "":
			result["tasks"] = []interface{}{taskTestRow(6, "OSS_TO_OSS", "FAILED"), taskTestRow(5, "OSS_TO_OSS", "FAILED")}
			result["nextBeforeId"], result["failedTasks"] = 5, 2
			result["totalPutObjectSize"] = int64(8 << 40)
		case "5":
			result["nextBeforeId"] = 3
		case "3":
			result["tasks"] = []interface{}{taskTestRow(2, "OSS_TO_OSS", "FAILED")}
			result["hasMore"], result["failedTasks"] = false, 1
			result["totalPutObjectSize"] = int64(1 << 40)
		default:
			t.Fatalf("repeated or wrong cursor %s", r.URL)
		}
		adminTestJSON(t, w, result)
	})
	result, err := taskList(client, []string{"--all", "--status", "failed", "--limit", "2"})
	if err != nil || !reflect.DeepEqual(calls, []string{"", "5", "3"}) || len(objectList(result["tasks"])) != 3 || result["hasMore"] != false || getInt(result, "failedTasks") != 3 || getInt64(result, "totalPutObjectSize") != 9<<40 {
		t.Fatal("all-page list or summary incomplete", result, calls, err)
	}
}

func TestTaskListAllRejectsRepeatedCursorAndRetainsReadResults(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprint(incomplete), func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				result := map[string]interface{}{"tasks": []interface{}{}, "hasMore": true, "nextBeforeId": 10}
				if calls == 1 {
					result["tasks"] = []interface{}{taskTestRow(10, "OSS_TO_OSS", "FAILED")}
				}
				if incomplete {
					result["partial"] = true
				}
				adminTestJSON(t, w, result)
			})
			result, err := taskList(client, []string{"--all"})
			if err == nil || result["partial"] != true || len(objectList(result["tasks"])) != 1 || calls > 2 {
				t.Fatal("invalid pagination silently accepted", result, calls, err)
			}
			if _, err := json.Marshal(result); err != nil {
				t.Fatal("partial evidence must remain JSON serializable", err)
			}
		})
	}
}

func TestTaskControlDirectLookupNeverFallsBackToUnrelatedCatalog(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			gets, posts := 0, 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets++
					if r.URL.Path != "/api/master/tasks/1" {
						t.Fatalf("read unrelated tasks %s", r.URL)
					}
					if missing {
						w.WriteHeader(http.StatusNotFound)
						adminTestJSON(t, w, map[string]interface{}{"error": "Task not found"})
						return
					}
					adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(1, "OSS_TO_OSS", "FAILED")})
					return
				}
				posts++
				body := adminTestBody(t, r)
				if r.URL.Path != "/api/master/retry" || !reflect.DeepEqual(body["expectedGenerations"], map[string]interface{}{"1": float64(1)}) {
					t.Fatal("unguarded retry", r.URL, body)
				}
				adminTestJSON(t, w, map[string]interface{}{"accepted": 1, "failed": 0})
			})
			_, err := taskControl(client, "retry", []string{"1"})
			if gets != 1 || missing && (err == nil || posts != 0) || !missing && (err != nil || posts != 1) {
				t.Fatal(gets, posts, err)
			}
		})
	}
}

func TestTaskRetryAllRequiresConfirmationAndMatchesWebFailureStates(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unconfirmed bulk retry made a request") })
	for _, args := range [][]string{{"--all"}, {"--all", "--yes", "1"}} {
		if _, err := taskControl(client, "retry", args); err == nil {
			t.Fatal("unsafe retry accepted", args)
		}
	}
	calls := 0
	client = adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == "GET" {
			if r.URL.Path != "/api/master/status" {
				t.Fatal("bulk selection did a per-ID catalog lookup", r.URL)
			}
			if r.URL.Query().Get("beforeId") == "" {
				rows := []interface{}{}
				for i, status := range []string{"RUNNING", "STOPPED", "RECOVERABLE", "RESUMING", "COMPLETED", "FAILED"} {
					rows = append(rows, taskTestRow(10-i, "OSS_TO_OSS", status))
				}
				adminTestJSON(t, w, map[string]interface{}{"tasks": rows, "hasMore": true, "nextBeforeId": 5})
			} else {
				adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(1, "LOCAL_TO_OSS", "PARTIAL_FAILED")}, "hasMore": false})
			}
			return
		}
		body := adminTestBody(t, r)
		if r.URL.Path != "/api/master/retry" || !reflect.DeepEqual(body["taskIds"], []interface{}{float64(5), float64(1)}) || len(body["expectedGenerations"].(map[string]interface{})) != 2 {
			t.Fatal("retry-all included stopped/active tasks or lost guard", r.URL, body)
		}
		adminTestJSON(t, w, map[string]interface{}{"accepted": 2, "failed": 0})
	})
	result, err := taskControl(client, "retry", []string{"--all", "--yes"})
	if err != nil || calls != 3 || getInt(result, "accepted") != 2 {
		t.Fatal(result, calls, err)
	}
}

func TestTaskRetryRejectsUnknownGenerationBeforeMutation(t *testing.T) {
	for _, generation := range []interface{}{nil, 0, -1, 1.5, "invalid"} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Fatal("unguarded retry submitted")
			}
			row := taskTestRow(1, "OSS_TO_OSS", "FAILED")
			row["generation"] = generation
			adminTestJSON(t, w, map[string]interface{}{"data": row})
		})
		if _, err := taskControl(client, "retry", []string{"1"}); err == nil {
			t.Fatal("invalid generation accepted", generation)
		}
	}
}

func TestTaskControlBatchesGuardEveryTaskAndStopOnUncertainResponse(t *testing.T) {
	var mu sync.Mutex
	gets, posts := 0, 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			mu.Lock()
			gets++
			mu.Unlock()
			id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/master/tasks/"))
			if err != nil {
				t.Error("not a direct lookup", r.URL)
				w.WriteHeader(500)
				return
			}
			row := taskTestRow(id, "OSS_TO_OSS", "FAILED")
			row["generation"] = 7
			adminTestJSON(t, w, map[string]interface{}{"data": row})
			return
		}
		posts++
		body := adminTestBody(t, r)
		ids := body["taskIds"].([]interface{})
		guards := body["expectedGenerations"].(map[string]interface{})
		if gets != 130 || len(ids) != 64 || len(guards) != 64 {
			t.Fatal("missing prechecks or unbounded POST", gets, len(ids), body)
		}
		for _, id := range ids {
			if guards[fmt.Sprint(id)] != float64(7) {
				t.Fatal("generation missing", id, guards)
			}
		}
		if posts == 2 {
			w.WriteHeader(http.StatusGatewayTimeout)
			adminTestJSON(t, w, map[string]interface{}{"error": "upstream timeout"})
			return
		}
		if posts > 2 {
			t.Fatal("uncertain mutation replayed or next batch sent")
		}
		adminTestJSON(t, w, map[string]interface{}{"accepted": 64, "failed": 0})
	})
	result, err := taskControl(client, "retry", []string{"1-130"})
	if err == nil || posts != 2 || getInt(result, "accepted") != 64 || getInt(result, "uncertain") != 64 || getInt(result, "unsubmitted") != 2 || !reflect.DeepEqual(result["unsubmittedIds"], []int{129, 130}) {
		t.Fatal("batch result lost acknowledgement boundaries", result, err)
	}
}

func TestTaskControlBatchesReturnPerTaskFailuresAndContinueDefiniteResults(t *testing.T) {
	ids := []int{}
	for i := 1; i <= 66; i++ {
		ids = append(ids, i)
	}
	posts := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts++
		batch := adminTestBody(t, r)["taskIds"].([]interface{})
		details := []interface{}{}
		for _, id := range batch {
			details = append(details, map[string]interface{}{"taskId": id, "success": id != float64(2), "accepted": id != float64(2), "message": "task state changed"})
		}
		adminTestJSON(t, w, map[string]interface{}{"details": details})
	})
	result, err := taskControlBatches(client, "stop", []taskControlGroup{{path: "/stop", ids: ids}}, nil)
	if err == nil || posts != 2 || getInt(result, "accepted") != 65 || !reflect.DeepEqual(result["failedIds"], []int{2}) || getInt(result, "uncertain") != 0 {
		t.Fatal("per-task failure hidden", result, err)
	}
}

func TestTaskRuntimeEndpointsAndChildPriorityParameters(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/tasks/44/progress" || r.URL.Query().Get("childNumber") != "2" || r.URL.Query().Get("generation") != "3" {
			t.Fatal(r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"data": map[string]interface{}{"parentTaskId": 44, "items": []interface{}{}, "partial": true}})
	})
	result, err := runTaskCommand(client, []string{"progress", "44", "--child", "44-2", "--generation", "3"})
	if err != nil || !incompleteResult(result) {
		t.Fatal(result, err)
	}
	client = adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/tasks/44" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(44, "OSS_TO_OSS", "RUNNING")})
			return
		}
		if r.URL.Path != "/api/master/tasks/44/children" || r.URL.Query().Get("order") != "priority" || r.URL.Query().Get("view") != "active" || r.URL.Query().Get("cursor") != "opaque:cursor" {
			t.Fatal(r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"data": map[string]interface{}{"items": []interface{}{}, "nextCursor": "next:cursor"}})
	})
	result, err = runTaskCommand(client, []string{"children", "44", "--active", "--cursor", "opaque:cursor"})
	page := result["subtaskPage"].(map[string]interface{})
	if err != nil || page["nextCursor"] != "next:cursor" || page["view"] != "active" || page["order"] != "priority" {
		t.Fatal(result, err)
	}
}

func TestTaskCleanupStatusBatchesAndPreservesMissingTasks(t *testing.T) {
	posts := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.URL.Path != "/api/master/cleanup-status" {
			t.Fatal(r.URL)
		}
		batch := adminTestBody(t, r)["taskIds"].([]interface{})
		if len(batch) > 128 {
			t.Fatal("unbounded cleanup page", len(batch))
		}
		rows := []interface{}{}
		for _, id := range batch {
			rows = append(rows, map[string]interface{}{"taskId": id, "missing": id == float64(130)})
		}
		adminTestJSON(t, w, map[string]interface{}{"tasks": rows})
	})
	result, err := runTaskCommand(client, []string{"cleanup-status", "1-130"})
	if err != nil || posts != 2 || result["cleanupStatus"] != true || len(objectList(result["tasks"])) != 130 || objectList(result["tasks"])[129]["missing"] != true {
		t.Fatal(result, err)
	}
}
