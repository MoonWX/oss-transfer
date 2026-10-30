package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func taskTestRow(id int, direction, status string) map[string]interface{} {
	return map[string]interface{}{"taskIndex": id, "direction": direction, "status": status, "generation": 1}
}

func TestTaskStopRoutesMixedDirectionsAfterDirectPrechecks(t *testing.T) {
	var mu sync.Mutex
	paths := []string{}
	rows := map[int]map[string]interface{}{224: taskTestRow(224, "OSS_TO_OSS", "RUNNING"), 225: taskTestRow(225, "OSS_TO_LOCAL", "PENDING"), 226: taskTestRow(226, "LOCAL_TO_OSS", "LIST_DONE")}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method == "GET" {
			id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/master/tasks/"))
			if err != nil || rows[id] == nil {
				t.Errorf("unexpected precheck %s", r.URL)
				w.WriteHeader(500)
				return
			}
			adminTestJSON(t, w, map[string]interface{}{"data": rows[id]})
			return
		}
		ids := adminTestBody(t, r)["taskIds"]
		switch r.URL.Path {
		case "/api/master/stop":
			if !reflect.DeepEqual(ids, []interface{}{float64(224)}) {
				t.Errorf("wrong OSS targets %#v", ids)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "failed": 0})
		case "/api/master/local-transfers/stop":
			if !reflect.DeepEqual(ids, []interface{}{float64(225), float64(226)}) {
				t.Errorf("wrong local targets %#v", ids)
			}
			adminTestJSON(t, w, map[string]interface{}{"data": map[string]interface{}{"accepted": 2, "failed": 0}})
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
		}
	})
	result, err := runTaskCommand(client, []string{"stop", "224-226"})
	if err != nil || result["success"] != true {
		t.Fatalf("control failed %#v %v", result, err)
	}
	sort.Strings(paths[:3])
	if !reflect.DeepEqual(paths, []string{"/api/master/tasks/224", "/api/master/tasks/225", "/api/master/tasks/226", "/api/master/stop", "/api/master/local-transfers/stop"}) {
		t.Fatal(paths)
	}
}

func TestTaskControlPrevalidatesAllTargetsBeforeAnyMutation(t *testing.T) {
	for _, invalid := range []string{"completed", "missing", "group-partial", "node-partial"} {
		t.Run(invalid, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				if r.Method != "GET" {
					t.Errorf("mutated before all selected tasks were validated")
					w.WriteHeader(500)
					return
				}
				id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/master/tasks/"))
				if err != nil {
					t.Errorf("not direct lookup %s", r.URL)
					w.WriteHeader(500)
					return
				}
				row := taskTestRow(id, "OSS_TO_OSS", "RUNNING")
				result := map[string]interface{}{"data": row}
				if id == 225 {
					switch invalid {
					case "completed":
						row["status"] = "COMPLETED"
					case "missing":
						w.WriteHeader(404)
						adminTestJSON(t, w, map[string]interface{}{"error": "Task not found"})
						return
					case "group-partial":
						row["snapshotComplete"] = false
						row["missingTaskIds"] = []int{230}
					case "node-partial":
						result["partial"] = true
						result["unreachableWorkers"] = []string{"10.0.0.3"}
					}
				}
				adminTestJSON(t, w, result)
			})
			if _, err := runTaskCommand(client, []string{"stop", "224", "225"}); err == nil {
				t.Fatal("invalid selection accepted")
			}
			if calls != 2 {
				t.Fatalf("requests = %d", calls)
			}
		})
	}
}

func TestTaskResumeAndRetryStatusAndDirectionContracts(t *testing.T) {
	for _, action := range []string{"resume", "retry"} {
		for _, direction := range []string{"OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"} {
			for _, state := range []string{"STOPPED", "FAILED", "PARTIAL_FAILED", "RUNNING", "PENDING", "COMPLETED", "STOPPING"} {
				t.Run(action+direction+state, func(t *testing.T) {
					allowed := state == "STOPPED" || state == "FAILED" || state == "PARTIAL_FAILED"
					writes := 0
					client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, direction, state)})
							return
						}
						writes++
						if !allowed {
							t.Fatal("non-resumable state mutated")
						}
						path := "/api/master/resume"
						if action == "retry" {
							path = "/api/master/retry"
						} else if direction != "OSS_TO_OSS" {
							path = "/api/master/local-transfers/resume"
						}
						if r.URL.Path != path {
							t.Fatalf("resume route = %s, want %s", r.URL.Path, path)
						}
						if !reflect.DeepEqual(adminTestBody(t, r)["taskIds"], []interface{}{float64(224)}) {
							t.Fatal("unexpected target IDs")
						}
						adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "failed": 0})
					})
					_, err := runTaskCommand(client, []string{action, "224"})
					if (err == nil) != allowed || (writes == 1) != allowed {
						t.Fatalf("allowed=%v writes=%d err=%v", allowed, writes, err)
					}
				})
			}
		}
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("resumed task during exploration")
		}
		row := taskTestRow(224, "OSS_TO_OSS", "STOPPED")
		row["ossTaskGroup"], row["creationStatus"] = true, "RUNNING"
		adminTestJSON(t, w, map[string]interface{}{"data": row})
	})
	if _, err := runTaskCommand(client, []string{"resume", "224"}); err == nil {
		t.Fatal("active exploration resumed")
	}
}

func TestTaskControlReturnsPartialFailure(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, "OSS_TO_LOCAL", "STOPPED")})
			return
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": 1, "failed": 1, "partial": true, "unreachableWorkers": []string{"10.0.0.3"}}})
	})
	result, err := runTaskCommand(client, []string{"resume", "224"})
	if err == nil || result["success"] != false {
		t.Fatalf("partial operation claimed success: %#v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("10.0.0.3")) {
		t.Fatal("failed node discarded")
	}
}

func TestTaskLegacyStartPartialFailureReturnsError(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, "OSS_TO_OSS", "PENDING")})
			return
		}
		if r.URL.Path != "/api/master/start" {
			t.Fatalf("unexpected start route: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "message": "启动任务已提交，成功: 1, 失败: 1", "details": []string{"节点 10.0.0.3: 转发失败"}})
	})
	result, err := runTaskCommand(client, []string{"start", "224"})
	if err == nil || result["success"] != false {
		t.Fatalf("legacy partial start accepted: %#v %v", result, err)
	}
}

func TestTaskBandwidthEndpointAndUnlimitedContract(t *testing.T) {
	for _, value := range []string{"", "200", "unlimited"} {
		t.Run(value, func(t *testing.T) {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/master/tasks/224/bandwidth-limit" {
					t.Fatalf("wrong endpoint: %s", r.URL)
				}
				if value == "" {
					if r.Method != "GET" {
						t.Fatalf("read used %s", r.Method)
					}
				} else {
					if r.Method != "PUT" {
						t.Fatalf("update used %s", r.Method)
					}
					body := adminTestBody(t, r)
					if _, present := body["limitMbps"]; !present {
						t.Fatal("missing limitMbps")
					}
					if value == "unlimited" {
						if body["limitMbps"] != nil {
							t.Fatal("unlimited must send null")
						}
					} else if body["limitMbps"] != float64(200) {
						t.Fatalf("rate converted incorrectly: %#v", body)
					}
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"limitMbps": 200}})
			})
			args := []string{"bandwidth", "224"}
			if value != "" {
				args = append(args, value)
			}
			if _, err := runTaskCommand(client, args); err != nil {
				t.Fatal(err)
			}
		})
	}
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid bandwidth wrote request") })
	for _, value := range []string{"0", "-1", "NaN", "Inf", "1000001", "invalid"} {
		if _, err := runTaskCommand(client, []string{"bandwidth", "224", value}); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestTaskReportUsesNativeZipEndpoint(t *testing.T) {
	var expected bytes.Buffer
	archive := zip.NewWriter(&expected)
	entry, err := archive.Create("summary.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("taskId,status\n224,COMPLETED\n")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/master/task-reports/224" {
			t.Fatalf("wrong report endpoint: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(expected.Bytes())
	})
	path := filepath.Join(t.TempDir(), "task.zip")
	result, err := runTaskCommand(client, []string{"report", "224", "--output", path})
	if err != nil || result["output"] != path {
		t.Fatalf("report result: %#v %v", result, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(content, expected.Bytes()) {
		t.Fatalf("report content changed: %v", err)
	}
}

func TestTaskChildIDsFollowWebUIOrderingWithoutMutatingInputs(t *testing.T) {
	children := []map[string]interface{}{
		{"taskIndex": float64(500), "shardId": "custom10"},
		{"taskIndex": float64(499), "shardId": "custom2"},
		{"taskIndex": float64(224), "shardId": "discovery"},
		{"taskIndex": float64(310), "shardId": "legacy-local-2"},
		{"taskIndex": float64(320), "shardIndex": float64(0)},
		{"taskIndex": float64(330)},
	}
	rows := numberCLIChildren(224, children, []interface{}{float64(320), float64(330)})
	wantIDs := []string{"224-1", "224-2", "224-3", "224-4", "224-5"}
	wantTasks := []int{320, 330, 310, 499, 500}
	if len(rows) != len(wantIDs) {
		t.Fatalf("discovery was counted as a child: %#v", rows)
	}
	for i, row := range rows {
		if row["childTaskId"] != wantIDs[i] || getInt(row, "taskIndex") != wantTasks[i] {
			t.Fatalf("row %d = %#v", i, row)
		}
	}
	for _, row := range children {
		if _, exists := row["childTaskId"]; exists {
			t.Fatal("child numbering mutated API input")
		}
	}
}

func TestTaskChildNumberingKeeps32BitBoundaryAndInvalidLegacyIndicesDistinct(t *testing.T) {
	children := []map[string]interface{}{
		{"taskIndex": float64(11), "shardIndex": float64(math.MaxInt32 - 1)},
		{"taskIndex": float64(12), "shardIndex": float64(math.MaxInt32)},
		{"taskIndex": float64(13), "shardId": "legacy-local-9999999999999999999999999"},
		{"taskIndex": float64(14), "shardIndex": float64(0)},
		{"taskIndex": float64(15), "shardIndex": int64(1<<32 + 1)},
	}
	rows := numberCLIChildren(224, children, nil)
	want := map[int]string{11: "224-2147483647", 12: "224-2", 13: "224-4", 14: "224-1", 15: "224-3"}
	for _, row := range rows {
		id := getInt(row, "taskIndex")
		if row["childTaskId"] != want[id] {
			t.Fatalf("child %d numbered as %v, want %s", id, row["childTaskId"], want[id])
		}
	}
}

func TestTaskShowPreservesGroupAndLocalDetailIncompleteness(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "group", true: "local"}[local], func(t *testing.T) {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/master/tasks/224" {
					row := taskTestRow(224, "OSS_TO_OSS", "STOPPED")
					if local {
						row["direction"] = "OSS_TO_LOCAL"
					} else {
						row["snapshotComplete"] = false
						row["missingTaskIds"] = []int{225}
					}
					adminTestJSON(t, w, map[string]interface{}{"data": row, "partial": false})
					return
				}
				if r.URL.Path != "/api/master/tasks/224/children" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("offset") != "0" {
					t.Fatalf("unexpected detail request: %s", r.URL)
				}
				page := map[string]interface{}{"parentTaskId": 224, "items": []interface{}{}, "totalCount": 0, "offset": 0, "limit": 25, "hasMore": false, "partial": false}
				if local {
					page["partial"], page["unreachableWorkers"] = true, []string{"10.0.0.3"}
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": page})
			})
			result, err := runTaskCommand(client, []string{"show", "224"})
			if err != nil {
				t.Fatal(err)
			}
			if !incompleteResult(result) {
				t.Fatalf("partial task claimed complete: %#v", result)
			}
			var output bytes.Buffer
			renderIncomplete(&output, result)
			want := "225"
			if local {
				want = "10.0.0.3"
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("partial detail diagnostics lost: %s", output.String())
			}
		})
	}
}

func TestTaskChildSelectionUsesExecutingNode(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		row := taskTestRow(224, "OSS_TO_LOCAL", "STOPPED")
		if r.URL.Path == "/api/master/tasks/224" {
			adminTestJSON(t, w, map[string]interface{}{"data": row})
			return
		}
		child := taskTestRow(225, "OSS_TO_LOCAL", "STOPPED")
		child["parentTaskId"] = 224
		child["shardIndex"] = 0
		child["childTaskId"] = "224-1"
		child["executionNodeIp"] = "10.0.0.2"
		child["eligibleWorkerNodes"] = []string{"10.0.0.2", "10.0.0.3"}
		if r.URL.Path != "/api/master/tasks/224/children/1" || r.URL.Query().Get("executionNodeIp") != "10.0.0.2" {
			t.Fatalf("child lookup loaded unrelated tasks: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": []interface{}{child}}})
	})
	result, err := runTaskCommand(client, []string{"show", "224", "--child", "224-1", "--node", "10.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := result["child"].(map[string]interface{})
	if child["childTaskId"] != "224-1" {
		t.Fatalf("wrong child: %#v", result)
	}
}

func TestNestedTaskCompletenessAndDiagnosticRendering(t *testing.T) {
	for _, marker := range []map[string]interface{}{{"partial": true}, {"snapshotComplete": false}, {"missingTaskIds": []int{225}}} {
		marker["taskIndex"] = 224
		for _, result := range []map[string]interface{}{{"tasks": []interface{}{marker}, "partial": false}, {"task": marker}, {"data": map[string]interface{}{"tasks": []map[string]interface{}{marker}}}, {"deletedTasks": map[string]interface{}{"10.0.0.2": []interface{}{marker}}}} {
			if !incompleteResult(result) {
				t.Fatalf("nested incompleteness ignored: %#v", result)
			}
		}
	}
	if incompleteResult(map[string]interface{}{"tasks": []interface{}{map[string]interface{}{"snapshotComplete": true, "missingTaskIds": []int{}}}}) {
		t.Fatal("complete empty group marked incomplete")
	}
	var out bytes.Buffer
	renderIncomplete(&out, map[string]interface{}{"task": map[string]interface{}{"taskIndex": 224, "snapshotComplete": false, "missingTaskIds": []int{225, 226}}, "localDetails": map[string]interface{}{"unreachableWorkers": []string{"10.0.0.3"}}})
	for _, want := range []string{"任务 #224", "225 226", "本地任务详情", "10.0.0.3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in diagnostics: %s", want, out.String())
		}
	}
}

func TestAdminTablesAndEnvelopeMetadataRemainVisible(t *testing.T) {
	for name, result := range map[string]map[string]interface{}{
		"node":     {"data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2", "label": "迁移节点", "master": true}}}, "workers": map[string]interface{}{"10.0.0.2": map[string]interface{}{"success": true, "taskStats": map[string]interface{}{"runningTasks": 3}}}},
		"storage":  {"configs": []interface{}{map[string]interface{}{"alias": "source", "endpoint": "https://oss.example", "secretKey": "MUST_NOT_PRINT", "accessKey": "MUST_NOT_PRINT"}}},
		"limit":    {"rateLimits": []interface{}{map[string]interface{}{"workerIp": "10.0.0.2", "bandwidth": map[string]interface{}{"manualLimitBps": float64(200e6), "limitBps": float64(100e6), "currentBps": float64(0), "state": "REDUCED"}}}},
		"envelope": {"data": map[string]interface{}{"valid": true, "nodeBindings": []interface{}{}}, "submissionStatus": "validated", "requestedCount": 1, "message": "尚未创建任务"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			renderCLIResult(&out, result)
			text := out.String()
			wants := map[string][]string{"node": {"10.0.0.2", "迁移节点", "在线", "主节点 / 执行节点", "连接状态"}, "storage": {"source", "已配置", "https://oss.example"}, "limit": {"200 Mbps", "100 Mbps", "0 Mbps", "已降速"}, "envelope": {"validated", "尚未创建任务", "请求数量"}}[name]
			for _, want := range wants {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			if strings.Contains(text, "MUST_NOT_PRINT") {
				t.Fatal("storage credentials leaked into table")
			}
		})
	}
}

func TestCLIBrokenSessionStillReturnsJSONAndCanLogout(t *testing.T) {
	dir := t.TempDir()
	original := sessionConfigDir
	sessionConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { sessionConfigDir = original })
	path, err := sessionPath("http://cli-test.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runMockCLI(t, []string{"whoami", "--json"}, func(http.ResponseWriter, *http.Request) { t.Fatal("broken session made network request") })
	var decoded map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &decoded) != nil || decoded["success"] != false || stderr == "" {
		t.Fatalf("broken session failure: %d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, []string{"logout", "--json"}, func(http.ResponseWriter, *http.Request) { t.Fatal("unreadable token sent to server") })
	if code != 3 || json.Unmarshal([]byte(out), &decoded) != nil || decoded["complete"] != false || !strings.Contains(out, "本机会话已删除") {
		t.Fatalf("logout recovery: %d %s %s", code, out, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("broken session retained: %v", err)
	}
}

func TestWatchSecondsRejectIntegerOverflowBeforeDurationConversion(t *testing.T) {
	for _, value := range []string{"36028797018963969", "9223372036854775807", "3601", "0", "-1"} {
		if _, err := parseGlobals([]string{"task", "list", "--watch", value}); err == nil {
			t.Errorf("accepted out-of-range seconds: %s", value)
		}
	}
	for _, value := range []string{"1", "3600", "5s"} {
		if _, err := parseGlobals([]string{"task", "list", "--watch", value}); err != nil {
			t.Errorf("rejected valid duration %s: %v", value, err)
		}
	}
}

func TestTaskListAcceptsCurrentAndCompatibleStatusFilters(t *testing.T) {
	for _, status := range []string{"RUNNING", "STOPPING", "RESUMING", "COMPLETED", "PARTIAL_FAILED", "FAILED", "STOPPED", "WAITING", "PENDING", "LIST_DONE", "RECOVERABLE"} {
		t.Run(status, func(t *testing.T) {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("status") != status {
					t.Fatalf("filter lost: %s", r.URL)
				}
				adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(1, "OSS_TO_OSS", status)}})
			})
			result, err := taskList(client, []string{"--status", strings.ToLower(status)})
			if err != nil || objectList(responseData(result)["tasks"])[0]["status"] != status {
				t.Fatalf("raw status changed: %#v %v", result, err)
			}
		})
	}
}

func TestTaskDeleteRejectsQueuedAndRecoveringTasksBeforeMutation(t *testing.T) {
	for _, status := range []string{"WAITING", "RECOVERABLE", "RESUMING"} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Fatalf("deleted active task %s", status)
			}
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(1, "OSS_TO_OSS", status)})
		})
		if _, err := runTaskCommand(client, []string{"delete", "1"}); err == nil {
			t.Fatalf("accepted %s", status)
		}
	}
}
