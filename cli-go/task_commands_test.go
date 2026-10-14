package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func taskTestRow(id int, direction, status string) map[string]interface{} {
	return map[string]interface{}{"taskIndex": id, "direction": direction, "status": status}
}

func TestTaskStopRoutesMixedDirectionsAfterSinglePrecheck(t *testing.T) {
	paths := []string{}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method == "GET" {
			if r.URL.Path != "/api/master/status" {
				t.Fatalf("unexpected precheck: %s", r.URL)
			}
			adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(224, "OSS_TO_OSS", "RUNNING"), taskTestRow(225, "OSS_TO_LOCAL", "PENDING"), taskTestRow(226, "LOCAL_TO_OSS", "LIST_DONE")}, "partial": false})
			return
		}
		if r.Method != "POST" {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		ids := adminTestBody(t, r)["taskIds"]
		switch r.URL.Path {
		case "/api/master/stop":
			if !reflect.DeepEqual(ids, []interface{}{float64(224)}) {
				t.Fatalf("wrong OSS targets: %#v", ids)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 1, "failed": 0})
		case "/api/master/local-transfers/stop":
			if !reflect.DeepEqual(ids, []interface{}{float64(225), float64(226)}) {
				t.Fatalf("wrong local targets: %#v", ids)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": 2, "failed": 0, "partial": false}})
		default:
			t.Fatalf("unexpected control request: %s", r.URL)
		}
	})
	result, err := runTaskCommand(client, []string{"stop", "224-226"})
	if err != nil || result["success"] != true {
		t.Fatalf("control failed: %#v, %v", result, err)
	}
	if !reflect.DeepEqual(paths, []string{"/api/master/status", "/api/master/stop", "/api/master/local-transfers/stop"}) {
		t.Fatalf("unexpected request sequence: %v", paths)
	}
}

func TestTaskControlPrevalidatesAllTargetsBeforeAnyMutation(t *testing.T) {
	for _, invalid := range []string{"completed", "missing", "group-partial", "node-partial"} {
		t.Run(invalid, func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					t.Fatalf("mutated first target before validating all: %s", r.URL)
				}
				first, second := taskTestRow(224, "OSS_TO_OSS", "RUNNING"), taskTestRow(225, "OSS_TO_LOCAL", "RUNNING")
				result := map[string]interface{}{"tasks": []interface{}{first, second}, "partial": false}
				switch invalid {
				case "completed":
					second["status"] = "COMPLETED"
				case "missing":
					second["taskIndex"] = 226
				case "group-partial":
					first["snapshotComplete"] = false
					first["missingTaskIds"] = []int{230}
				case "node-partial":
					result["partial"] = true
					result["unreachableWorkers"] = []string{"10.0.0.3"}
				}
				adminTestJSON(t, w, result)
			})
			if _, err := runTaskCommand(client, []string{"stop", "224", "225"}); err == nil {
				t.Fatal("invalid batch accepted")
			}
			if calls != 1 {
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
							adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(224, direction, state)}})
							return
						}
						writes++
						if !allowed {
							t.Fatal("non-resumable state mutated")
						}
						path := "/api/master/resume"
						if direction != "OSS_TO_OSS" {
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
		adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{row}})
	})
	if _, err := runTaskCommand(client, []string{"resume", "224"}); err == nil {
		t.Fatal("active exploration resumed")
	}
}

func TestTaskControlReturnsPartialFailure(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(224, "OSS_TO_LOCAL", "STOPPED")}})
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
			adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(224, "OSS_TO_OSS", "PENDING")}})
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
