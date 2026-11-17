package master

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// A Master restart while one of a task's nodes is down used to turn "last
// connected at …" into "has not connected yet", hiding how long the node had
// been gone. Its last connection is now kept across the restart; a node with
// no record is reported as not connected since this Master started. Neither
// counts as online until it connects to the restarted Master.
func TestOfflineTaskNodesKeepTheirLastConnectionAcrossAMasterRestart(t *testing.T) {
	f := newAgentHTTPFixture(t)
	code, session := f.join(t, f.invite(t, "down.example"), "down-agent", agentCredential(t))
	if code != 200 {
		t.Fatal("join", code)
	}
	// A later poll, as one within the save interval is kept in memory; the
	// Master saves it when it stops.
	f.master.agents.mu.Lock()
	later := f.master.agents.lastSeen[session.NodeID].Add(30 * time.Second)
	f.master.agents.lastSeen[session.NodeID] = later
	f.master.agents.mu.Unlock()
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{
		{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "down.example"},
		{ID: "never-seen", EnrollmentID: "enrolled", Address: "never.example"},
	}
	p, _, err := f.master.tasks.repo.Create(spec, "last-seen-across-restart")
	if err != nil {
		t.Fatal(err)
	}

	f.close()
	before := time.Now()
	f.open(t)
	after := time.Now()
	code, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	row := body["data"].(map[string]any)
	if attention, _ := row["attention"].(map[string]any); attention["code"] != "NODES_OFFLINE" {
		t.Fatal("attention", row["attention"])
	}
	offline, _ := row["offlineNodes"].([]any)
	if len(offline) != 2 {
		t.Fatal("offline nodes", offline)
	}
	for _, raw := range offline {
		node := raw.(map[string]any)
		switch node["executionNodeIp"] {
		case "down.example":
			if node["lastSeen"] != float64(later.UnixMilli()) || node["notConnectedSince"] != nil {
				t.Fatal("the node lost its last connection in the restart", node, later.UnixMilli())
			}
		case "never.example":
			// Not connected since the restarted Master began accepting Agents.
			since, _ := node["notConnectedSince"].(float64)
			if node["lastSeen"] != nil || since < float64(before.UnixMilli()) || since > float64(after.UnixMilli()) {
				t.Fatal("a node without a record", node, before.UnixMilli(), after.UnixMilli())
			}
		default:
			t.Fatal("unexpected node", node)
		}
	}
	nodes := f.nodes(t)
	if len(nodes) != 1 {
		t.Fatal("registry", nodes)
	}
	registered := nodes[0].(map[string]any)
	if registered["online"] != false || registered["lastSeen"] != later.UTC().Format(time.RFC3339Nano) {
		t.Fatal("the registry invented a connection or lost the last one", registered, later.UTC().Format(time.RFC3339Nano))
	}
	if online, err := f.master.onlineAgents(); err != nil || len(online) != 0 {
		t.Fatal("a saved time made a node online", online, err)
	}
}
