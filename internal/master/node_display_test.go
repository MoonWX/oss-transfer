package master

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestNodeDisplayUsesNamesAndActualIPWithoutChangingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, address, label, remote string
		info                         nodeRuntimeInfo
		want                         NodeDisplay
	}{
		{"named", "agent-4fddb1cb", "训练节点", "10.1.0.43", nodeRuntimeInfo{}, NodeDisplay{"训练节点", "10.1.0.43", "训练节点 (10.1.0.43)", false}},
		{"hostname", "agent-4fddb1cb", "", "10.1.0.43", nodeRuntimeInfo{hostname: "storage-a"}, NodeDisplay{"storage-a", "10.1.0.43", "storage-a (10.1.0.43)", false}},
		{"same host", "agent-4fddb1cb", "storage-a", "127.0.0.1", nodeRuntimeInfo{ips: []string{"127.0.0.1", "0.0.0.0", "10.1.0.43"}}, NodeDisplay{"storage-a", "10.1.0.43", "storage-a (10.1.0.43)", false}},
		{"identity is not IP", "agent-4fddb1cb", "", "not-an-IP", nodeRuntimeInfo{}, NodeDisplay{"", "", "Node ID: agent-4fddb1cb", true}},
		{"no invented hostname", "removed-node.example", "", "", nodeRuntimeInfo{}, NodeDisplay{"", "", "Node ID: removed-node.example", true}},
		{"legacy IP", "10.1.0.43", "", "", nodeRuntimeInfo{}, NodeDisplay{"", "10.1.0.43", "10.1.0.43", false}},
		{"IPv6", "agent-old", "IPv6 worker", "2001:db8::43", nodeRuntimeInfo{}, NodeDisplay{"IPv6 worker", "2001:db8::43", "IPv6 worker (2001:db8::43)", false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := makeNodeDisplay(tc.address, tc.label, tc.remote, tc.info); got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestNodeDisplayResolvesExistingTasksAndRetainsRevokedNames(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "display.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := task.New(db)
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: "node-one", EnrollmentID: "enrollment-one", Address: "agent-4fddb1cb"}}
	parent, _, err := repo.Create(spec, "existing-task")
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Assign(parent.ID, parent.Generation, 1, []transferstore.Object{{Key: "one.bin", Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: db, agents: &agentControl{remoteAddress: map[string]string{"node-one": "10.1.0.43"}}, tasks: &taskRuntime{repo: repo}}
	member := cluster.Member{ID: "node-one", Address: "agent-4fddb1cb", Label: "训练节点", CredentialHash: "must-not-be-exposed"}
	put := func() {
		t.Helper()
		if err := db.Update(func(tx *state.Tx) error { return tx.Put("agent-members", member.ID, member) }); err != nil {
			t.Fatal(err)
		}
	}
	put()
	initial := s.nodeDisplaySnapshot()
	member.Label, member.Revoked = "已移除节点", true
	put()
	displays := s.nodeDisplaySnapshot()
	if initial.lookup(member.ID, member.Address).DisplayLabel != "训练节点 (10.1.0.43)" {
		t.Fatal("snapshot changed with the registry")
	}
	for _, row := range []map[string]any{s.taskView(parent, displays), s.childView(parent, child, displays)} {
		if row["executionNodeLabel"] != "已移除节点 (10.1.0.43)" || row["executionNodeAddress"] != "10.1.0.43" || row["executionNodeIdentityOnly"] != false {
			t.Fatalf("historical node was not resolved: %#v", row)
		}
		nodes := row["executionNodes"].([]map[string]any)
		if len(nodes) != 1 || nodes[0]["id"] != "node-one" || nodes[0]["address"] != "agent-4fddb1cb" {
			t.Fatal("routing identity changed", nodes)
		}
	}
	if row := s.childView(parent, child, displays); row["workerNode"] != member.Address || row["executionNodeIp"] != member.Address {
		t.Fatal("legacy request selectors changed", row)
	}
	stored, err := repo.Get(parent.ID)
	if err != nil || !reflect.DeepEqual(stored.Spec.Nodes, spec.Nodes) {
		t.Fatal("display enrichment modified the saved task", stored.Spec.Nodes, err)
	}
	encoded, err := json.Marshal(displays.byAddress)
	if err != nil || strings.Contains(string(encoded), member.CredentialHash) {
		t.Fatal("node display exposed credentials", err)
	}
	// A restarted Master has no in-memory peer addresses; the saved label still
	// identifies a removed node without inventing an IP from its Agent key.
	s.agents.remoteAddress = nil
	afterRestart := s.nodeDisplaySnapshot().lookup(member.ID, member.Address)
	if afterRestart.DisplayLabel != member.Label || afterRestart.DisplayAddress != "" {
		t.Fatal("revoked name was lost after restart", afterRestart)
	}
}
