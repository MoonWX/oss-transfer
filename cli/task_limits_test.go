package main

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

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
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"limitMbps": 100, "requestLimits": map[string]interface{}{"scope": "PER_NODE", "headQps": 100}}})
			})
			result, err := runTaskCommand(client, append([]string{"limits", "224"}, tc.args...))
			if err != nil || calls != 1 {
				t.Fatal("limits operation must use one request", err, calls)
			}
			var rendered bytes.Buffer
			renderCLIResult(&rendered, result)
			if !strings.Contains(rendered.String(), "PER_NODE") {
				t.Fatal("legacy per-node request limit scope was hidden")
			}
		})
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
