package master

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// Changing a task's limits while one of its nodes is offline saves them,
// applies them on the connected node at once and says that the offline node
// receives them when it reconnects. The change used to wait thirty seconds
// for each offline node in turn and then fail with HTTP 409 and "Agent …
// bandwidth: context deadline exceeded", although it had been saved.
func TestTaskLimitsChangeWithAnOfflineNodeIsSavedAndReportedPerNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, live := taskAPIAgent(t, f)
	code, session := f.join(t, f.invite(t, "offline.example"), "offline-agent", agentCredential(t))
	if code != 200 {
		t.Fatal("join", code)
	}
	f.master.agents.mu.Lock()
	delete(f.master.agents.lastSeen, session.NodeID)
	f.master.agents.mu.Unlock()
	offline := task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "offline.example"}
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{live, offline}
	parent, _, err := f.master.tasks.repo.Create(spec, "limits-with-offline-node")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, parent.ID)
	code, _, body, err := localizedRequest(f.http.Client(), "PUT", url, f.admin, "zh-CN", map[string]any{"limitMbps": 100, "requestLimitsPatch": map[string]any{"putQps": 200}})
	if err != nil || code != http.StatusOK || body["message"] != "任务限速已保存" {
		t.Fatal("saved limits reported as a failure", code, body, err)
	}
	data := body["data"].(map[string]any)
	limits, _ := data["requestLimits"].(map[string]any)
	if data["limitMbps"] != float64(100) || limits["putQps"] != float64(200) || data["applied"] != false || data["pending"] != false {
		t.Fatal("task limits", data)
	}
	nodes := data["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatal("nodes", nodes)
	}
	for _, raw := range nodes {
		node := raw.(map[string]any)
		switch node["address"] {
		case live.Address:
			if node["applied"] != true {
				t.Fatal("the connected node did not apply the limits", node)
			}
		case offline.Address:
			if node["applied"] != false || node["pending"] != false || node["message"] != "执行节点未连接，重新连接后自动应用" {
				t.Fatal("the offline node", node)
			}
		default:
			t.Fatal("unexpected node", node)
		}
	}
	stored, err := f.master.tasks.repo.Get(parent.ID)
	if err != nil || stored.Spec.BandwidthMbps != 100 || stored.Spec.RequestLimits == nil || stored.Spec.RequestLimits.PutQPS != 200 {
		t.Fatal("limits were not saved", stored.Spec, err)
	}
	expected, ok := stored.NodePolicyFor(live.ID)
	policy, err := service.Limits.Parent(parent.ID)
	if !ok || err != nil || policy.Revision != expected.Revision || policy.BandwidthBytesPerSecond != expected.BandwidthBytesPerSecond || policy.PutQPS != expected.PutQPS {
		t.Fatal("the connected node holds other limits", policy, expected, err)
	}
}
