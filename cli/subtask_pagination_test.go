package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCLISubtasksReadOnlyRequestedNumberWindow(t *testing.T) {
	for _, direction := range []string{"OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"} {
		t.Run(direction, func(t *testing.T) {
			requests := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path == "/api/master/tasks/224" {
					if r.URL.RawQuery != "" {
						t.Fatal("loaded embedded child task payload")
					}
					adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, direction, "RUNNING"), "partial": false})
					return
				}
				if r.Method != "GET" || r.URL.Path != "/api/master/tasks/224/children" || r.URL.Query().Get("offset") != "25" || r.URL.Query().Get("limit") != "25" {
					t.Fatalf("read full/unrelated children: %s %s", r.Method, r.URL)
				}
				// Preserve a numbering hole and a replica; never renumber a page relative to its offset.
				items := []interface{}{
					map[string]interface{}{"childTaskId": "224-26", "subtaskNumber": 26, "taskIndex": -1, "executionNodeIp": "10.0.0.2", "direction": direction},
					map[string]interface{}{"childTaskId": "224-28", "subtaskNumber": 28, "taskIndex": -2, "executionNodeIp": "10.0.0.2", "direction": direction},
					map[string]interface{}{"childTaskId": "224-28", "subtaskNumber": 28, "taskIndex": -3, "executionNodeIp": "10.0.0.3", "direction": direction},
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"parentTaskId": 224, "items": items, "totalCount": 80, "offset": 25, "limit": 25, "hasMore": true, "partial": false}})
			})
			result, err := runTaskCommand(client, []string{"children", "224", "--offset", "25", "--limit", "25"})
			if err != nil || requests != 2 || incompleteResult(result) {
				t.Fatalf("paged query failed/claimed partial: %#v %v calls=%d", result, err, requests)
			}
			rows := objectList(result["children"])
			if len(rows) != 3 || rows[0]["childTaskId"] != "224-26" || rows[2]["childTaskId"] != "224-28" {
				t.Fatalf("page numbering changed or duplicate hidden: %#v", result)
			}
			var output bytes.Buffer
			renderCLIResult(&output, result)
			for _, want := range []string{"#224-26", "#224-28", "子任务编号 26–50 / 80", "task children 224 --offset 50 --limit 25"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("missing %q: %s", want, output.String())
				}
			}
		})
	}
}

func TestCLIChildLookupIsDirectAndRejectsAmbiguousReplicas(t *testing.T) {
	for _, selected := range []bool{false, true} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/master/tasks/224" {
				adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, "OSS_TO_OSS", "STOPPED")})
				return
			}
			if r.URL.Path != "/api/master/tasks/224/children/128" {
				t.Fatalf("did not directly lookup requested child: %s", r.URL)
			}
			items := []interface{}{}
			for _, worker := range []string{"10.0.0.2", "10.0.0.3"} {
				if node := r.URL.Query().Get("executionNodeIp"); node != "" && node != worker {
					continue
				}
				items = append(items, map[string]interface{}{"childTaskId": "224-128", "executionNodeIp": worker, "taskIndex": -128, "eligibleWorkerNodes": []interface{}{"10.0.0.2", "10.0.0.3"}})
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"parentTaskId": 224, "items": items, "partial": false}})
		})
		args := []string{"show", "224", "--child", "#224-128"}
		if selected {
			args = append(args, "--node", "10.0.0.3")
		}
		result, err := runTaskCommand(client, args)
		if !selected {
			if err == nil || !strings.Contains(err.Error(), "指定 --node") {
				t.Fatalf("silently picked a replica: %#v %v", result, err)
			}
		} else if err != nil || taskWorkers(result["child"].(map[string]interface{})) != "10.0.0.3" {
			t.Fatalf("explicit node failed: %#v %v", result, err)
		}
	}
}

func TestCLISubtaskPageValidationBeforeNetworkAndPartialDiagnostics(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid selector made request") })
	for _, args := range [][]string{{"children", "224", "--limit", "201"}, {"children", "224", "--offset", "-1"}, {"show", "224", "--child", "225-1"}, {"show", "224", "--child", "224-0"}, {"show", "224", "--child", "224-1", "--offset", "0"}, {"children", "224", "--child", "224-1"}, {"show", "224", "--node", "10.0.0.2"}} {
		if _, err := runTaskCommand(client, args); err == nil {
			t.Fatalf("invalid child selector accepted: %v", args)
		}
	}
	code, output, stderr := runMockCLI(t, []string{"task", "children", "224", "--json"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/tasks/224" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, "OSS_TO_LOCAL", "RUNNING")})
			return
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"parentTaskId": 224, "items": []interface{}{}, "offset": 0, "limit": 25, "totalCount": 100, "hasMore": true, "partial": true, "unreachableWorkers": []string{"10.0.0.3"}}})
	})
	var result map[string]interface{}
	if code != 3 || json.Unmarshal([]byte(output), &result) != nil || !strings.Contains(stderr, "10.0.0.3") || result["partial"] != true {
		t.Fatalf("child-page incompleteness discarded: %d %s %s", code, output, stderr)
	}
}

func TestCLIChildMalformedPageDoesNotLookEmpty(t *testing.T) {
	for _, malformed := range []interface{}{nil, "not-list", map[string]interface{}{"taskIndex": 1}, []interface{}{"not-an-object"}} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/master/tasks/224" {
				adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(224, "OSS_TO_OSS", "RUNNING")})
				return
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": malformed}})
		})
		if result, err := runTaskCommand(client, []string{"children", "224"}); err == nil || !incompleteResult(result) {
			t.Fatal(fmt.Sprintf("malformed page became an empty success: %#v %v", result, err))
		}
	}
}

func TestCLIEmptyChildWindowUsesOrdinalUpperBound(t *testing.T) {
	for _, offset := range []int{25, 100} {
		result := map[string]interface{}{
			"children":    []interface{}{},
			"subtaskPage": map[string]interface{}{"parentTaskId": 224, "totalCount": 80, "offset": offset, "limit": 25, "hasMore": offset < 80},
		}
		var output bytes.Buffer
		renderCLIResult(&output, result)
		if !strings.Contains(output.String(), "此编号范围无记录，最大编号 80") || strings.Contains(output.String(), "共 80") {
			t.Fatalf("maximum ordinal was represented as a record count: %s", output.String())
		}
	}
}

// In the active view the Master returns the subtasks in progress after child
// number --offset, their count and the last number read (nextOffset). The
// printed next-page commands must visit every subtask in progress once, and
// each footer must name the numbers actually shown.
func TestCLIActiveSubtaskPagesFollowTheNextChildNumber(t *testing.T) {
	active := []int{}
	for number := 3001; number <= 3040; number++ {
		active = append(active, number)
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/tasks/5" {
			adminTestJSON(t, w, map[string]interface{}{"data": taskTestRow(5, "OSS_TO_OSS", "RUNNING")})
			return
		}
		query := r.URL.Query()
		if r.URL.Path != "/api/master/tasks/5/children" || query.Get("view") != "active" || query.Has("order") {
			t.Fatalf("unexpected request %s", r.URL)
		}
		after, _ := strconv.Atoi(query.Get("offset"))
		limit, _ := strconv.Atoi(query.Get("limit"))
		items, next := []interface{}{}, after
		for _, number := range active {
			if number > after && len(items) < limit {
				items = append(items, map[string]interface{}{"childTaskId": fmt.Sprintf("5-%d", number), "subtaskNumber": number, "taskIndex": 5, "status": "RUNNING"})
				next = number
			}
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"parentTaskId": 5, "offset": after, "limit": limit, "totalCount": len(active), "items": items, "hasMore": next < active[len(active)-1], "nextOffset": next, "partial": false}})
	})
	seen := map[string]int{}
	args := []string{"children", "5", "--active", "--offset", "0"}
	footers := []string{}
	for page := 0; page < 5; page++ {
		result, err := runTaskCommand(client, args)
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range objectList(result["children"]) {
			seen[getString(child, "childTaskId")]++
		}
		var output bytes.Buffer
		renderCLIResult(&output, result)
		text := output.String()
		if strings.Contains(text, "此编号范围无记录") || strings.Contains(text, "子任务编号") {
			t.Fatalf("active page was numbered by position:\n%s", text)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "本页") {
				footers = append(footers, line)
			}
		}
		_, command, found := strings.Cut(text, "下一页：task ")
		if !found {
			break
		}
		args = strings.Fields(strings.SplitN(command, "\n", 2)[0])
	}
	if len(seen) != len(active) {
		t.Fatalf("next-page commands showed %d of %d subtasks in progress: %v", len(seen), len(active), seen)
	}
	for child, count := range seen {
		if count != 1 {
			t.Fatalf("subtask %s shown %d times", child, count)
		}
	}
	want := []string{"本页 25 个进行中的子任务（编号 3001–3025），共 40 个进行中", "本页 15 个进行中的子任务（编号 3026–3040），共 40 个进行中"}
	if !reflect.DeepEqual(footers, want) {
		t.Fatalf("footers = %q", footers)
	}
	// Past the last subtask in progress the footer says so instead of naming
	// a position range.
	result, err := runTaskCommand(client, []string{"children", "5", "--active", "--offset", "3040"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	renderCLIResult(&output, result)
	if !strings.Contains(output.String(), "编号 3040 之后没有进行中的子任务，共 40 个进行中") || strings.Contains(output.String(), "下一页") {
		t.Fatal(output.String())
	}
}
