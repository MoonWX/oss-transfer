package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// taskLimitsTestNode is one entry of the nodes list in the Master's task
// limits response, with the display fields the Master adds.
func taskLimitsTestNode(address string, applied, pending bool, message string) map[string]interface{} {
	node := map[string]interface{}{"address": address, "displayName": "", "displayAddress": address, "displayLabel": address, "identityOnly": false, "applied": applied, "pending": pending}
	if message != "" {
		node["message"] = message
	}
	return node
}

// taskLimitsTestResponse mirrors GET/PUT /tasks/{id}/bandwidth-limit.
func taskLimitsTestResponse(limitMbps, requestLimits interface{}, applied, pending bool, nodes ...interface{}) map[string]interface{} {
	return map[string]interface{}{"success": true, "data": map[string]interface{}{
		"limitMbps": limitMbps, "requestLimits": requestLimits, "applied": applied, "pending": pending,
		"sharedTaskCount": 1, "canEdit": true, "allocation": "dynamic", "shareCommits": 3, "nodes": nodes,
	}}
}

func renderedTaskLimits(locale cliLocale, result map[string]interface{}) string {
	var out bytes.Buffer
	renderCLIResult(&cliLocaleWriter{Writer: &out, locale: locale}, result)
	return strings.Join(strings.Fields(out.String()), " ")
}

func TestTaskLimitsUpdatesOnlySuppliedFieldsWithoutReadingOldValues(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body map[string]interface{}
	}{
		{nil, nil},
		{[]string{"--list-qps", "20.5"}, map[string]interface{}{"requestLimitsPatch": map[string]interface{}{"listQps": 20.5}}},
		{[]string{"--put-qps", "unlimited"}, map[string]interface{}{"requestLimitsPatch": map[string]interface{}{"putQps": float64(0)}}},
		{[]string{"--head-qps", "0", "--put-qps", "250", "--bandwidth-mbps", "100"}, map[string]interface{}{"limitMbps": float64(100), "requestLimitsPatch": map[string]interface{}{"headQps": float64(0), "putQps": float64(250)}}},
		{[]string{"--bandwidth-mbps", "unlimited"}, map[string]interface{}{"limitMbps": nil}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/master/tasks/224/bandwidth-limit" {
					t.Fatal(r.URL.Path)
				}
				if tc.body == nil {
					if r.Method != "GET" {
						t.Fatal("query unexpectedly mutated task limits")
					}
				} else if r.Method != "PUT" || !reflect.DeepEqual(adminTestBody(t, r), tc.body) {
					t.Fatal("limits update read or overwrote unspecified fields")
				}
				adminTestJSON(t, w, taskLimitsTestResponse(100, map[string]interface{}{"scope": "PER_NODE", "headQps": 100}, true, false, taskLimitsTestNode("10.0.0.2", true, false, "")))
			})
			result, err := runTaskCommand(client, append([]string{"limits", "224"}, tc.args...))
			if err != nil || calls != 1 {
				t.Fatal("limits operation must use one request", err, calls)
			}
			rendered := renderedTaskLimits(localeZH, result)
			if !strings.Contains(rendered, "请求上限范围 每个执行节点") || !strings.Contains(rendered, "HEAD QPS 100") {
				t.Fatal("legacy per-node request limit scope was hidden:", rendered)
			}
		})
	}
}

func TestTaskLimitsAndBandwidthQueriesShowTaskLimitsNotNodes(t *testing.T) {
	response := taskLimitsTestResponse(100, map[string]interface{}{"scope": "CLUSTER", "listQps": 20.5, "putQps": 200}, false, false,
		map[string]interface{}{"address": "10.0.0.2", "displayName": "迁移节点", "displayAddress": "10.0.0.2", "displayLabel": "迁移节点 (10.0.0.2)", "identityOnly": false, "applied": true, "pending": false,
			"demand": "busy", "adjusting": false, "share": map[string]interface{}{"bandwidthBytesPerSecond": 12500000, "listQps": 20.5, "headQps": 0, "putQps": 200}},
		taskLimitsTestNode("10.0.0.3", false, false, "执行节点未连接"))
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/master/tasks/224/bandwidth-limit" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		adminTestJSON(t, w, response)
	}
	for _, command := range []string{"limits", "bandwidth"} {
		code, out, stderr := runMockCLI(t, []string{"task", command, "224"}, handler)
		if code != 0 || stderr != "" {
			t.Fatalf("task %s: %d %s %s", command, code, out, stderr)
		}
		normalized := strings.Join(strings.Fields(out), " ")
		for _, want := range []string{"任务带宽 100 Mbps", "请求上限范围 所有执行节点合计", "LIST / GET QPS 20.5", "HEAD QPS 仅受节点上限约束", "PUT QPS 200", "生效状态 部分节点未生效", "10.0.0.3: 执行节点未连接"} {
			if !strings.Contains(normalized, want) {
				t.Errorf("task %s output misses %q: %s", command, want, out)
			}
		}
		// No node registry table, and the applied node is not listed as a failure.
		for _, unwanted := range []string{"连接状态", "个执行节点", "迁移节点"} {
			if strings.Contains(out, unwanted) {
				t.Errorf("task %s output contains %q: %s", command, unwanted, out)
			}
		}
		code, out, stderr = runMockCLI(t, []string{"task", command, "224", "--json"}, handler)
		var decoded struct {
			Success bool `json:"success"`
			Data    struct {
				LimitMbps     float64                  `json:"limitMbps"`
				RequestLimits map[string]interface{}   `json:"requestLimits"`
				Applied       *bool                    `json:"applied"`
				Nodes         []map[string]interface{} `json:"nodes"`
			} `json:"data"`
		}
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &decoded) != nil {
			t.Fatalf("task %s --json: %d %s %s", command, code, out, stderr)
		}
		if !decoded.Success || decoded.Data.LimitMbps != 100 || decoded.Data.RequestLimits["listQps"] != 20.5 || decoded.Data.Applied == nil || *decoded.Data.Applied || len(decoded.Data.Nodes) != 2 || decoded.Data.Nodes[1]["message"] != "执行节点未连接" {
			t.Fatalf("task %s --json changed the Master response: %s", command, out)
		}
	}
}

func TestTaskLimitsShowWhetherEveryNodeApplied(t *testing.T) {
	for _, test := range []struct {
		name           string
		result         map[string]interface{}
		want, unwanted []string
	}{
		{"applied without limits", taskLimitsTestResponse(nil, nil, true, false, taskLimitsTestNode("10.0.0.2", true, false, "")),
			[]string{"任务带宽 仅受节点上限约束", "请求上限范围 所有执行节点合计", "LIST / GET QPS 仅受节点上限约束", "HEAD QPS 仅受节点上限约束", "PUT QPS 仅受节点上限约束", "生效状态 所有执行节点已生效"},
			[]string{"10.0.0.2", "旧版带宽上限", "未生效"}},
		{"pending", taskLimitsTestResponse(1000000, map[string]interface{}{"scope": "CLUSTER", "headQps": 0.000001}, false, true, taskLimitsTestNode("10.0.0.2", false, true, "")),
			[]string{"任务带宽 1000000 Mbps", "HEAD QPS 0.000001", "生效状态 正在同步到执行节点"},
			[]string{"10.0.0.2", "未生效"}},
		{"legacy limits partly applied", taskLimitsTestResponse(0.5, map[string]interface{}{"scope": "PER_NODE", "listQps": 10, "bandwidthBitsPerSecond": 80000000}, false, false,
			taskLimitsTestNode("10.0.0.2", true, false, ""), taskLimitsTestNode("10.0.0.3", false, false, ""), taskLimitsTestNode("10.0.0.4", false, true, "")),
			[]string{"任务带宽 0.5 Mbps", "请求上限范围 每个执行节点", "LIST / GET QPS 10", "旧版带宽上限 80 Mbps", "生效状态 部分节点未生效", "10.0.0.3: 未生效"},
			[]string{"10.0.0.2", "10.0.0.4"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := renderedTaskLimits(localeZH, test.result)
			for _, want := range test.want {
				if !strings.Contains(rendered, want) {
					t.Errorf("missing %q: %s", want, rendered)
				}
			}
			for _, unwanted := range test.unwanted {
				if strings.Contains(rendered, unwanted) {
					t.Errorf("unexpected %q: %s", unwanted, rendered)
				}
			}
		})
	}
	legacy := taskLimitsTestResponse(nil, map[string]interface{}{"scope": "PER_NODE", "bandwidthBitsPerSecond": 80000000}, false, false, taskLimitsTestNode("10.0.0.3", false, false, ""))
	for locale, want := range map[cliLocale][]string{
		localeEN: {"Task bandwidth Node limits only", "Request limit scope Per execution node", "Legacy bandwidth limit 80 Mbps", "Application status Not applied on all nodes", "10.0.0.3: Not applied"},
		localeJA: {"タスク帯域幅 ノードの上限に従う", "リクエスト上限の適用範囲 実行ノードごと", "旧版の帯域幅上限 80 Mbps", "適用状況 一部のノードで未適用", "10.0.0.3: 未適用"},
	} {
		rendered := renderedTaskLimits(locale, legacy)
		for _, text := range want {
			if !strings.Contains(rendered, text) {
				t.Errorf("%s missing %q: %s", locale, text, rendered)
			}
		}
	}
	// task show prints the same request limit rows.
	var detail bytes.Buffer
	renderTaskDetail(&detail, map[string]interface{}{"taskIndex": 224, "status": "RUNNING", "requestLimits": map[string]interface{}{"scope": "PER_NODE", "listQps": 10}})
	if text := strings.Join(strings.Fields(detail.String()), " "); !strings.Contains(text, "请求上限范围 每个执行节点 LIST / GET QPS 10 HEAD QPS 仅受节点上限约束 PUT QPS 仅受节点上限约束") {
		t.Fatalf("task show request limits changed: %s", detail.String())
	}
}

func TestTaskLimitsRejectInvalidValuesBeforeAnyRequest(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid limit made API request") })
	for _, value := range []string{"-1", "NaN", "Inf", "1000000001", "0.0000001", "invalid"} {
		if _, err := runTaskCommand(client, []string{"limits", "224", "--list-qps", value}); err == nil {
			t.Errorf("invalid QPS accepted: %q", value)
		}
		if _, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--put-qps", value}); err == nil {
			t.Errorf("invalid creation QPS accepted: %q", value)
		}
	}
	if !watchable([]string{"task", "limits", "224"}) || watchable([]string{"task", "limits", "224", "--put-qps", "10"}) {
		t.Fatal("watch must never repeat a limit update")
	}
}

func TestTaskCreateIncludesClusterRequestLimits(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange(), creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"taskIds":[224]}`,
		inspect: func(rows []map[string]interface{}) {
			want := map[string]interface{}{"scope": "CLUSTER", "listQps": float64(10), "headQps": 20.5, "putQps": float64(0)}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0]["requestLimits"], want) {
				t.Fatalf("task request limits = %v", rows)
			}
		},
	})
	if _, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--list-qps", "10", "--head-qps", "20.5", "--put-qps", "unlimited"}); err != nil {
		t.Fatal(err)
	}
}
