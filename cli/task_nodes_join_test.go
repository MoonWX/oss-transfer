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
	english := renderTaskNodesForTest(result, localeEN)
	if !strings.Contains(english, "Not joined yet: ") {
		t.Fatalf("English membership output:\n%s", english)
	}

	result, err = runTaskCommand(client, []string{"nodes", "12", "--remove", "10.0.0.2", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "尚未加入: 执行节点 gamma (10.0.0.4) 离线") {
		t.Fatalf("accepted change output:\n%s", text)
	}

	delete(nodes[2], "joinProblem")
	result, err = runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); strings.Contains(text, "尚未加入") {
		t.Fatalf("a join without a reason printed one:\n%s", text)
	}
}
