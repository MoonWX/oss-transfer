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

func TestNodeAddressesSurviveMasterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	member := cluster.Member{ID: "node-local", Address: "jpfs-oss"}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("agent-members", member.ID, member) }); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"hostname": "jpfs-oss", "localIps": []any{"127.0.0.1", "10.1.0.43"}}
	before := &Server{store: db, agents: &agentControl{remoteAddress: map[string]string{}, nodeInfo: map[string]nodeRuntimeInfo{}}}
	before.rememberNodeInfo(member.ID, report)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info := map[string]nodeRuntimeInfo{}
	if err := loadNodeInfo(db, info); err != nil {
		t.Fatal(err)
	}
	// After a restart the co-located Agent reconnects over loopback before it
	// reports its addresses again.
	after := &Server{store: db, agents: &agentControl{remoteAddress: map[string]string{member.ID: "::1"}, nodeInfo: info}}
	if got := after.nodeDisplaySnapshot().lookup(member.ID, member.Address).DisplayLabel; got != "jpfs-oss (10.1.0.43)" {
		t.Fatalf("label after restart = %q", got)
	}

	// A changed address replaces the stored one.
	after.rememberNodeInfo(member.ID, map[string]any{"hostname": "jpfs-oss", "localIps": []any{"10.1.0.44"}})
	reloaded := map[string]nodeRuntimeInfo{}
	if err := loadNodeInfo(db, reloaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded[member.ID], nodeRuntimeInfo{"jpfs-oss", []string{"10.1.0.44"}}) {
		t.Fatalf("stored info = %#v", reloaded[member.ID])
	}
}

func TestUnsavedNodeAddressesAreRetried(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := &Server{store: db, agents: &agentControl{remoteAddress: map[string]string{}, nodeInfo: map[string]nodeRuntimeInfo{}}}
	s.rememberNodeInfo("node-one", map[string]any{"hostname": "storage-a", "localIps": []any{"10.1.0.43"}})
	if _, kept := s.agents.nodeInfo["node-one"]; kept {
		t.Fatal("an unsaved report would suppress the next identical write")
	}
}
