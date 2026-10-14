package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestTaskHistoryPaginationAndValidation(t *testing.T) {
	for _, action := range []string{"list", "trash"} {
		t.Run(action, func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Query().Get("beforeId") != "150" || r.URL.Query().Get("limit") != "20" {
					t.Errorf("pagination was not passed to API: %s", r.URL)
				}
				result := map[string]interface{}{"success": true, "partial": true, "truncated": true, "nextBeforeId": 130}
				if action == "list" {
					if r.URL.Path != "/api/master/status" {
						t.Errorf("unexpected endpoint: %s", r.URL.Path)
					}
					result["tasks"] = []interface{}{}
				} else {
					if r.URL.Path != "/api/master/deleted-tasks" {
						t.Errorf("unexpected endpoint: %s", r.URL.Path)
					}
					result["deletedTasks"] = map[string]interface{}{}
				}
				adminTestJSON(t, w, result)
			})
			result, err := runTaskCommand(client, []string{action, "--before-id", "150", "--limit", "20", "--search", "older"})
			if err != nil || getInt(result, "nextBeforeId") != 130 {
				t.Fatalf("lost page boundary: %#v, %v", result, err)
			}
			var output bytes.Buffer
			renderCLIResult(&output, result)
			if !strings.Contains(output.String(), "task "+action+" --before-id 130") {
				t.Fatalf("missing next-page instructions: %s", output.String())
			}
			for _, args := range [][]string{{"--limit", "0"}, {"--limit", "10001"}, {"--before-id", "-1"}, {"--before-id", "9223372036854775808"}, {"--before-id", "1", "--before-id", "2"}} {
				if _, err := runTaskCommand(client, append([]string{action}, args...)); err == nil {
					t.Fatalf("invalid page boundary accepted: %v", args)
				}
			}
			if calls != 1 {
				t.Fatalf("invalid query reached the server: %d calls", calls)
			}
		})
	}
}

func TestExplicitTaskHistoryPageHasSuccessfulExit(t *testing.T) {
	for _, action := range []string{"list", "trash"} {
		for _, partial := range []bool{false, true} {
			code, output, stderr := runMockCLI(t, []string{"task", action, "--limit", "20", "--json"}, func(w http.ResponseWriter, r *http.Request) {
				result := map[string]interface{}{"success": true, "partial": partial, "complete": !partial, "pageComplete": true, "hasMore": true, "truncated": true, "nextBeforeId": 130}
				if action == "list" {
					result["tasks"] = []interface{}{}
				} else {
					result["deletedTasks"] = map[string]interface{}{}
				}
				adminTestJSON(t, w, result)
			})
			var result map[string]interface{}
			wantCode := 0
			if partial {
				wantCode = 3
			}
			if code != wantCode || json.Unmarshal([]byte(output), &result) != nil || getInt(result, "nextBeforeId") != 130 || !partial && stderr != "" {
				t.Fatalf("%s partial=%v: code=%d stdout=%s stderr=%s", action, partial, code, output, stderr)
			}
		}
	}
}
