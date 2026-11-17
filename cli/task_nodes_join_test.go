package main

import (
	"strings"
	"testing"
)

// task nodes prints why a joining node has not joined yet, in the Master's
// words, both when it shows the members and after a change was accepted.
// Only joining nodes carry a reason; older Masters send none.
func TestTaskNodesShowsWhyANodeHasNotJoined(t *testing.T) {
	view := taskNodesTestView(false)
	nodes := objectList(view["nodes"])
	nodes[2]["joinProblem"] = map[string]interface{}{"code": "AGENT_OFFLINE", "message": "执行节点 gamma (10.0.0.4) 离线，恢复连接后继续加入"}
	// A stale reason on a node that has joined meanwhile is not shown.
	nodes[0]["joinProblem"] = map[string]interface{}{"code": "AGENT_OFFLINE", "message": "执行节点 alpha (10.0.0.1) 离线，恢复连接后继续加入"}
	master := &taskNodesFakeMaster{view: view}
	client := adminTestClient(t, master.handler(t))
	result, err := runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	text := renderTaskNodesForTest(result, localeZH)
	if !strings.Contains(text, "尚未加入: 执行节点 gamma (10.0.0.4) 离线，恢复连接后继续加入\n") || strings.Contains(text, "alpha (10.0.0.1) 离线") {
		t.Fatalf("membership output:\n%s", text)
	}
	// The command that cancels the join follows its reason.
	if !strings.Contains(text, "  取消加入: oss-transfer-cli task nodes 12 --remove 10.0.0.4 --yes\n") || strings.Count(text, "取消加入: ") != 1 {
		t.Fatalf("membership output lacks the cancellation:\n%s", text)
	}
	english := renderTaskNodesForTest(result, localeEN)
	if !strings.Contains(english, "Not joined yet: ") || !strings.Contains(english, "  Cancel the join: oss-transfer-cli task nodes 12 --remove 10.0.0.4 --yes\n") {
		t.Fatalf("English membership output:\n%s", english)
	}

	result, err = runTaskCommand(client, []string{"nodes", "12", "--remove", "10.0.0.2", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "尚未加入: 执行节点 gamma (10.0.0.4) 离线") {
		t.Fatalf("accepted change output:\n%s", text)
	}

	// A task that cannot be changed now still says why, without a command.
	view["editable"], view["reason"] = false, "任务正在停止"
	result, err = runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "尚未加入: 执行节点 gamma") || strings.Contains(text, "取消加入: ") {
		t.Fatalf("an uneditable task offered a cancellation:\n%s", text)
	}
	view["editable"], view["reason"] = true, ""

	delete(nodes[2], "joinProblem")
	result, err = runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); strings.Contains(text, "尚未加入") {
		t.Fatalf("a join without a reason printed one:\n%s", text)
	}
}
