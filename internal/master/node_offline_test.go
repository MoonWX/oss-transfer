package master

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// joinOfflineAgents registers count Agents that never poll. Even ones were
// last seen an hour ago; odd ones have not connected since the Master
// started. Their names sort before the Agents a test runs.
func joinOfflineAgents(t *testing.T, f *agentHTTPFixture, count int) map[string]bool {
	t.Helper()
	offline := map[string]bool{}
	for i := range count {
		name := fmt.Sprintf("a-offline-%02d", i)
		code, session := f.join(t, f.invite(t, name+".example"), name, agentCredential(t))
		if code != 200 {
			t.Fatal("join", name, code)
		}
		f.master.agents.mu.Lock()
		if i%2 == 0 {
			f.master.agents.lastSeen[session.NodeID] = f.master.options.Now().UTC().Add(-time.Hour)
		} else {
			delete(f.master.agents.lastSeen, session.NodeID)
		}
		f.master.agents.mu.Unlock()
		offline[name+".example"] = true
	}
	return offline
}

// Registered Agents that stopped polling must not decide whether the connected
// ones are heard. A request to an Agent that does not poll waits for the whole
// deadline and holds one of the eight shared slots; the connected Agents
// queued behind them were reported as failed, unavailable or without a rate
// change, although they were never asked.
func TestOfflineAgentsAreNotAskedAndDoNotCrowdOutConnectedAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	offline := joinOfflineAgents(t, f, 10)
	var agents []*nodeFixture
	for _, name := range []string{"z-live-a", "z-live-b", "z-live-c"} {
		agents = append(agents, nodeTestAgent(t, f, name))
	}
	call := func(method, path, language string, body any) map[string]any {
		t.Helper()
		code, _, response, err := localizedRequest(f.http.Client(), method, f.http.URL+path, f.admin, language, body)
		if err != nil || code != 200 {
			t.Fatal(method, path, code, response, err)
		}
		return response
	}
	expectedMissing := make([]any, 0, len(offline))
	for address := range offline {
		expectedMissing = append(expectedMissing, address)
	}
	sort.Slice(expectedMissing, func(i, j int) bool { return expectedMissing[i].(string) < expectedMissing[j].(string) })
	sameAddresses := func(got any) bool {
		list, _ := got.([]any)
		return fmt.Sprint(list) == fmt.Sprint(expectedMissing)
	}

	status := call("GET", "/api/master/worker-status", "zh-CN", nil)
	workers := status["workers"].(map[string]any)
	for _, agent := range agents {
		if worker, _ := workers[agent.address].(map[string]any); worker["success"] != true || worker["offline"] != nil {
			t.Fatalf("connected Agent %s was reported as %v", agent.address, worker)
		}
	}
	for address := range offline {
		if worker, _ := workers[address].(map[string]any); worker["success"] != false || worker["offline"] != true || worker["message"] != "节点离线" {
			t.Fatalf("offline Agent %s was reported as %v", address, worker)
		}
	}
	if !sameAddresses(status["failedWorkers"]) {
		t.Fatal("failed workers are not exactly the offline Agents", status["failedWorkers"])
	}

	local := call("GET", "/api/master/local-nodes", "en-US", nil)
	nodes := local["data"].(map[string]any)["nodes"].([]any)
	if len(nodes) != len(offline)+len(agents) {
		t.Fatal("local nodes", nodes)
	}
	for _, raw := range nodes {
		node := raw.(map[string]any)
		if offline[node["executionNodeIp"].(string)] {
			if node["reachable"] != false || node["offline"] != true || node["message"] != "Agent is offline" {
				t.Fatal("offline Agent offered for selection", node)
			}
		} else if node["reachable"] != true || node["offline"] != nil {
			t.Fatal("connected Agent not offered for selection", node)
		}
	}

	rates := call("POST", "/api/master/rate-limit/put", "", map[string]any{"qps": 100})
	if rates["successCount"] != float64(len(agents)) || rates["failedCount"] != float64(len(offline)) || rates["code"] != "RATE_UPDATE_UNCONFIRMED" {
		t.Fatal("rate update counts", rates)
	}
	for _, raw := range rates["results"].([]any) {
		result := raw.(map[string]any)
		if offline[result["workerIp"].(string)] {
			if result["success"] != false || result["offline"] != true || result["message"] != "Agent is offline" {
				t.Fatal("offline Agent rate result", result)
			}
		} else if result["success"] != true {
			t.Fatal("connected Agent did not confirm the rate", result)
		}
	}
	for _, agent := range agents {
		snapshot, err := agent.service.Nodes.Snapshot()
		if err != nil || snapshot["put"].(map[string]any)["limitQps"] != float64(100) {
			t.Fatal("connected Agent did not receive the rate", agent.address, snapshot, err)
		}
	}

	history := call("GET", "/api/master/rate-limit/history?limit=50", "", nil)
	if !sameAddresses(history["failedWorkers"]) || history["complete"] != false {
		t.Fatal("history misses connected Agents or lists offline ones as answered", history)
	}
	answered := map[any]bool{}
	for _, raw := range history["adjustments"].([]any) {
		answered[raw.(map[string]any)["workerIp"]] = true
	}
	for _, agent := range agents {
		if !answered[agent.address] {
			t.Fatal("history lacks the rate change of a connected Agent", agent.address, history)
		}
	}
}
