package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSetupUsesBootstrapAndPreservesPasswordWhitespace(t *testing.T) {
	t.Setenv("QA_SETUP_TOKEN", "test-bootstrap")
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/setup/complete" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Internal-Token") != "" {
			t.Fatal("setup reused administrative credentials")
		}
		body := adminTestBody(t, r)
		if body["setupToken"] != "test-bootstrap" || body["password"] != "  correct password  " || body["publicUrl"] != "https://transfer.example" || body["username"] != "operator" {
			t.Fatal("setup body was changed")
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	client.authToken = "old-login"
	_, err := runSetupCommand(client, []string{"complete", "--username", "operator", "--public-url", "https://transfer.example/", "--setup-token-env", "QA_SETUP_TOKEN", "--password-stdin"}, strings.NewReader("  correct password  \n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range [][]string{
		{"--password-stdin", "--setup-token-stdin"},
		{"--password-env", "QA_SETUP_TOKEN", "--password-stdin", "--setup-token-env", "QA_SETUP_TOKEN"},
		{"--password-env", "QA_SETUP_TOKEN", "--setup-token", "unsafe-argv-secret"},
	} {
		client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("ambiguous secrets sent") })
		args := append([]string{"complete", "--username", "admin", "--public-url", "https://transfer.example"}, tail...)
		if _, err := runSetupCommand(client, args, strings.NewReader("secret")); err == nil {
			t.Fatal("accepted unsafe secret options")
		}
	}
}

func TestAgentWindowsAndPosixRootsArePassedWithoutHostPathCleaning(t *testing.T) {
	for _, root := range []string{`C:\`, `D:\Data Files`, `\\server\share\files`, "/mnt/data with spaces "} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/master/nodes/storage-node" || r.Method != "PUT" {
				t.Fatal(r.URL)
			}
			body := adminTestBody(t, r)
			if len(body) != 1 || fmt.Sprint(body["advertisedLocalRoots"]) != fmt.Sprint([]string{root}) {
				t.Fatal(body)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true})
		})
		if _, err := runAgentAdmin(client, "update", []string{"storage-node", "--local-root", root}); err != nil {
			t.Fatal(err)
		}
		bindings, err := createParseBindings([]string{"storage-node=" + root})
		if err != nil || bindings[0].(map[string]interface{})["localPath"] != root {
			t.Fatal(bindings, err)
		}
	}
	for _, bad := range []string{"C:", `C:relative`, `\rooted`, `\\server`, `\\?\C:\data`, "relative", "/tmp\x00invalid"} {
		if agentAbsolutePath(bad) {
			t.Fatalf("accepted relative/device path %q", bad)
		}
	}
	for _, valid := range []string{"node-one", "node.example", "::1", "2001:db8::2", "10.0.0.2"} {
		if err := adminNodeAddress(valid); err != nil {
			t.Fatal(valid, err)
		}
	}
}

func TestDirectTaskLookupOnlyFallsBackFor404(t *testing.T) {
	for _, status := range []int{401, 404, 500} {
		calls := 0
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				if r.URL.Path != "/api/master/tasks/123456" {
					t.Fatal(r.URL)
				}
				w.WriteHeader(status)
				adminTestJSON(t, w, map[string]interface{}{"message": "failed"})
				return
			}
			if status != 404 || r.URL.Path != "/api/master/status" {
				t.Fatal("unexpected history fallback")
			}
			adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{taskTestRow(123456, "OSS_TO_OSS", "STOPPED")}})
		})
		_, task, err := taskByID(client, 123456)
		if status == 404 {
			if err != nil || task == nil || calls != 2 {
				t.Fatal(task, err, calls)
			}
		} else if err == nil || calls != 1 {
			t.Fatal("auth/storage failure hidden", err, calls)
		}
	}
}

func TestGoExceptionQueryTrustsExactScopeAndEmptyFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			kind := "failed"
			if fallback {
				kind = "fallback"
			}
			if r.URL.Path != "/api/master/"+kind || r.Method != "POST" {
				t.Fatal("unrelated cluster query", r.URL)
			}
			body := adminTestBody(t, r)
			if body["workerNode"] != "node-one" {
				t.Fatal(body)
			}
			key := "failedKeys"
			if fallback {
				key = "fallbackObjects"
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "complete": true, "partial": false, "supported": !fallback,
				"queriedWorkers": []string{"node-one"}, key: map[string]interface{}{"node-one": []interface{}{}, "node-oneCount": 0}})
		})
		kind := "failed"
		if fallback {
			kind = "fallback"
		}
		result, err := runExceptionsAdmin(client, "list", []string{kind, "--worker", "node-one", "--task-id", "123"})
		if err != nil || incompleteResult(result) {
			t.Fatal(result, err)
		}
	}
}

func TestGoNodeRateTablePreservesUnitsAndOfflineNodes(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, map[string]interface{}{"success": true, "partial": true, "failedWorkers": []string{"offline"}, "workers": map[string]interface{}{
			"node-one": map[string]interface{}{"success": true, "rateLimit": map[string]interface{}{"bandwidth": map[string]interface{}{"limitBps": 100e6}}},
			"offline":  map[string]interface{}{"success": false},
		}})
	})
	result, err := runLimitAdmin(client, "list", nil)
	if err != nil || !incompleteResult(result) {
		t.Fatal(result, err)
	}
	rows := adminArray(result["rateLimits"])
	if len(rows) != 1 || rows[0].(map[string]interface{})["workerIp"] != "node-one" {
		t.Fatal(rows)
	}
	if rows[0].(map[string]interface{})["bandwidth"].(map[string]interface{})["limitBps"] != float64(100e6) {
		t.Fatal("bandwidth unit changed")
	}
}
