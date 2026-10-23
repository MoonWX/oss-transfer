package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const displayNodeID = "agent-4fddb1cb"
const displayNodeLabel = "Training data (10.1.0.43)"

func testNodeDisplay() map[string]interface{} {
	return map[string]interface{}{"displayName": "Training data", "displayAddress": "10.1.0.43", "displayLabel": displayNodeLabel}
}

func testDisplayedTask() map[string]interface{} {
	node := testNodeDisplay()
	node["id"], node["address"] = "enrollment-id", displayNodeID
	return map[string]interface{}{
		"taskIndex": 1, "parentTaskId": 1, "status": "RUNNING", "direction": "LOCAL_TO_OSS",
		"eligibleWorkerNodes": []interface{}{displayNodeID}, "executionNodes": []interface{}{node},
		"sourceEndpoint": map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{
			map[string]interface{}{"executionNodeIp": displayNodeID, "localPath": "/mnt/data"},
		}},
	}
}

func TestNodeDisplayTaskRowsDetailsAndTrashPreserveIdentities(t *testing.T) {
	parent := testDisplayedTask()
	child := withCLINodeDisplay(parent, displayNodeID, cliNodeDisplays{displayNodeID: testNodeDisplay()})
	child["childTaskId"], child["executionNodeIp"] = "1-2", displayNodeID
	for _, result := range []map[string]interface{}{
		{"tasks": []interface{}{parent}}, {"task": parent}, {"task": parent, "child": child},
		{"children": []interface{}{child}}, {"recycledTasks": []interface{}{parent}},
	} {
		before, _ := json.Marshal(result)
		var out bytes.Buffer
		renderCLIResult(&out, result)
		if !strings.Contains(out.String(), displayNodeLabel) || strings.Contains(out.String(), displayNodeID) {
			t.Fatalf("task output did not use the node label: %s", &out)
		}
		after, _ := json.Marshal(result)
		if !bytes.Equal(before, after) || taskWorkers(parent) != displayNodeID || taskWorkerNode(child) != displayNodeID {
			t.Fatal("rendering changed a node selector or API data")
		}
	}
	if got := taskEndpoint(parent, true); got != displayNodeLabel+":/mnt/data" {
		t.Fatal(got)
	}
	if got := taskEndpoint(child, true); got != "/mnt/data" {
		t.Fatalf("display metadata changed child binding selection: %s", got)
	}
}

func TestNodeDisplayCommandsAndAliasesUseEnrichmentWithoutExtraLookups(t *testing.T) {
	for _, args := range [][]string{{"node", "list"}, {"worker-status"}, {"list-worker"}, {"agent", "show", displayNodeID}} {
		requests := 0
		code, out, stderr := runMockCLI(t, args, func(w http.ResponseWriter, r *http.Request) {
			requests++
			switch r.URL.Path {
			case "/api/master/nodes":
				node := testNodeDisplay()
				node["address"], node["agent"], node["online"] = displayNodeID, true, true
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{node}}})
			case "/api/master/worker-status":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "workers": map[string]interface{}{displayNodeID: map[string]interface{}{"success": true}}})
			default:
				t.Fatalf("unexpected request: %s", r.URL)
			}
		})
		if code != 0 || stderr != "" || !strings.Contains(out, displayNodeLabel) || !strings.Contains(out, displayNodeID) || requests != 2 {
			t.Fatalf("%v: code=%d requests=%d output=%s errors=%s", args, code, requests, out, stderr)
		}
	}
}

func TestNodeDisplayChildSelectorAndTaskFilterRemainRaw(t *testing.T) {
	for _, args := range [][]string{{"task", "list", "--worker", displayNodeID}, {"list-task", "--worker", displayNodeID}, {"task", "show", "1", "--child", "1-2", "--node", displayNodeID}} {
		requests := 0
		code, out, stderr := runMockCLI(t, args, func(w http.ResponseWriter, r *http.Request) {
			requests++
			switch r.URL.Path {
			case "/api/master/status":
				if r.URL.Query().Get("workerNode") != displayNodeID {
					t.Fatalf("task filter sent a display label: %s", r.URL)
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "tasks": []interface{}{testDisplayedTask()}})
			case "/api/master/tasks/1":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": testDisplayedTask()})
			case "/api/master/tasks/1/children/2":
				if r.URL.Query().Get("executionNodeIp") != displayNodeID {
					t.Fatalf("child selector sent a display label: %s", r.URL)
				}
				child := testDisplayedTask()
				child["childTaskId"], child["executionNodeIp"] = "1-2", displayNodeID
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": []interface{}{child}}})
			default:
				t.Fatalf("unexpected request: %s", r.URL)
			}
		})
		if code != 0 || stderr != "" || !strings.Contains(out, displayNodeLabel) || requests > 2 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
	}
}

func TestNodeDisplayRatesHistoryAndPartialResponses(t *testing.T) {
	for _, args := range [][]string{{"limit", "list"}, {"rate-limit"}, {"limit", "history"}, {"rate-limit-history"}} {
		requests := 0
		code, out, stderr := runMockCLI(t, args, func(w http.ResponseWriter, r *http.Request) {
			requests++
			result := map[string]interface{}{"success": true, "nodeDisplays": map[string]interface{}{displayNodeID: testNodeDisplay()}}
			switch r.URL.Path {
			case "/api/master/rate-limit":
				result["workers"] = map[string]interface{}{displayNodeID: map[string]interface{}{"success": true, "rateLimit": map[string]interface{}{"put": map[string]interface{}{"limitQps": 50}}}}
			case "/api/master/rate-limit/history":
				result["adjustments"] = []interface{}{map[string]interface{}{"workerIp": displayNodeID, "reason": "congestion"}}
			default:
				t.Fatalf("unexpected request: %s", r.URL)
			}
			adminTestJSON(t, w, result)
		})
		if code != 0 || stderr != "" || !strings.Contains(out, displayNodeLabel) || requests != 1 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
	}
	result := map[string]interface{}{
		"nodeDisplays":  map[string]interface{}{displayNodeID: testNodeDisplay(), "agent-other": testNodeDisplay()},
		"failedWorkers": []string{displayNodeID}, "workerErrors": map[string]interface{}{displayNodeID: "first error", "agent-other": "second error"},
	}
	var out bytes.Buffer
	renderIncomplete(&out, result)
	for _, want := range []string{displayNodeLabel, "first error", "second error"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("lost node information: %s", &out)
		}
	}
}

func TestNodeDisplayExceptionExportKeepsSelectorsAndShowsNameAndIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.csv")
	requests := 0
	code, _, stderr := runMockCLI(t, []string{"exceptions", "export", "failed", "--worker", displayNodeID, "--output", path}, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/master/failed" || adminTestBody(t, r)["workerNode"] != displayNodeID {
			t.Fatalf("exception selector changed or extra request: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{
			"success": true, "complete": true, "queriedWorkers": []string{displayNodeID},
			"nodeDisplays": map[string]interface{}{displayNodeID: testNodeDisplay()},
			"failedKeys":   map[string]interface{}{displayNodeID: []interface{}{map[string]interface{}{"key": "sample.bin"}}, displayNodeID + "Count": 1},
		})
	})
	if code != 0 || stderr != "" || requests != 1 {
		t.Fatalf("export: code=%d requests=%d errors=%s", code, requests, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff"))).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("invalid CSV: %s %v", data, err)
	}
	fields := map[string]string{}
	for index, key := range rows[0] {
		fields[key] = rows[1][index]
	}
	if fields["workerNode"] != displayNodeLabel || fields["nodeId"] != displayNodeID || len(fields) != 19 {
		t.Fatalf("CSV lost identity or display metadata: %v", fields)
	}
}

func TestNodeDisplayUnknownIdentityIsNotAnIP(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		if got := cliNodeLabel(nil, "10.1.0.43", locale); got != "10.1.0.43" {
			t.Fatal(got)
		}
		for _, row := range []map[string]interface{}{nil, {"displayLabel": displayNodeID, "identityOnly": true}, {"executionNodeLabel": displayNodeID}, {"executionNodeLabel": "Node ID: " + displayNodeID, "executionNodeIdentityOnly": true}} {
			got := cliNodeLabel(row, displayNodeID, locale)
			if got == displayNodeID || !strings.Contains(got, displayNodeID) || strings.Contains(got, "IP") {
				t.Fatalf("unknown identity rendered as an IP or unlabeled node: %s", got)
			}
			if locale == localeZH && strings.Contains(got, "Node ID") {
				t.Fatalf("unknown row did not localize its identity-only fallback: %s", got)
			}
		}
	}
	display := map[string]interface{}{"identityOnly": true, "displayLabel": "Node ID: " + displayNodeID}
	row := withCLINodeDisplay(map[string]interface{}{"workerNode": displayNodeID}, displayNodeID, cliNodeDisplays{displayNodeID: display})
	if row["executionNodeIdentityOnly"] != true || cliNodeLabel(row, displayNodeID, localeZH) != "节点标识 "+displayNodeID {
		t.Fatalf("flattened historical row lost its identity-only marker: %v", row)
	}
	display["address"] = displayNodeID
	var out bytes.Buffer
	renderCLIResult(&out, map[string]interface{}{"node": display})
	if strings.Contains(out.String(), "Node ID") || !strings.Contains(out.String(), "节点标识 "+displayNodeID) {
		t.Fatalf("generic node details did not localize unknown identity: %s", &out)
	}
}
