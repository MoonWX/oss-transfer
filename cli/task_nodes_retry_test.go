package main

import (
	"strings"
	"testing"
)

// Cancelling a drain on a local transfer keeps the node's path: no binding is
// required or sent, and no preflight runs because nothing joins.
func TestTaskNodesLocalDrainCancelNeedsNoBinding(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	client := adminTestClient(t, master.handler(t))
	if _, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.2", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if len(master.posts) != 1 || master.posts[0]["dryRun"] != nil || len(master.posts[0]["bindings"].([]interface{})) != 0 {
		t.Fatalf("drain cancel must submit without bindings or preflight: %v", master.posts)
	}
	master.posts = nil
	code, _, stderr := runMockCLI(t, []string{"task", "nodes", "12", "--add", "10.0.0.2", "--binding", "10.0.0.2=/data/b", "--yes"}, master.handler(t))
	if code != 2 || len(master.posts) != 0 || !strings.Contains(stderr, "--binding") {
		t.Fatalf("binding for a draining node must be a usage error: %d %v %s", code, master.posts, stderr)
	}
}

// A retry after an unknown outcome replays the stored result; a fresh dry run
// would compare against the already changed membership and fail.
func TestTaskNodesLocalRetrySkipsPreflightAndReplays(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	master.view["nodeRevision"] = 4
	master.respond = func(body map[string]interface{}) (int, interface{}) {
		if body["dryRun"] == true {
			return 409, map[string]interface{}{"success": false, "code": "STALE", "message": "stale"}
		}
		if body["requestId"] != "cli-retry-local" || body["expectedNodeRevision"] != float64(3) {
			return 409, map[string]interface{}{"success": false, "code": "CONFLICT", "message": "conflict"}
		}
		return 200, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": true, "replayed": true, "valid": true, "view": master.view}}
	}
	client := adminTestClient(t, master.handler(t))
	result, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share", "--request-id", "cli-retry-local", "--expected-revision", "3", "--yes"})
	if err != nil || len(master.posts) != 1 {
		t.Fatal(err, master.posts)
	}
	if data := responseData(result); data["replayed"] != true {
		t.Fatalf("retry did not reach the replay: %v", result)
	}
}

// Cancelling an immediate removal that is not fenced yet also keeps the path.
func TestTaskNodesLocalRemovingCancelNeedsNoBinding(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	nodes := master.view["nodes"].([]interface{})
	nodes[1].(map[string]interface{})["state"], nodes[1].(map[string]interface{})["drainMode"] = "REMOVING", "now"
	client := adminTestClient(t, master.handler(t))
	if _, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.2", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if len(master.posts) != 1 || master.posts[0]["dryRun"] != nil {
		t.Fatalf("cancelling an immediate removal must submit without preflight: %v", master.posts)
	}
}

// Retrying an accepted drain cancel: the node now shows ACTIVE, but the CLI
// must still send the original request so the Master can replay it.
func TestTaskNodesRetryOfAcceptedCancelReachesReplay(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	nodes := master.view["nodes"].([]interface{})
	nodes[1].(map[string]interface{})["state"], nodes[1].(map[string]interface{})["drainMode"] = "ACTIVE", ""
	master.respond = func(body map[string]interface{}) (int, interface{}) {
		return 200, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": true, "replayed": true, "valid": true, "view": master.view}}
	}
	client := adminTestClient(t, master.handler(t))
	result, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.2", "--request-id", "r1", "--expected-revision", "3", "--yes"})
	if err != nil || len(master.posts) != 1 || responseData(result)["replayed"] != true {
		t.Fatalf("retry did not reach the replay: %v %v %v", err, master.posts, result)
	}
}
