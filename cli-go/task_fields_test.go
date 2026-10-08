package main

import "testing"

func taskFieldBindings() []interface{} {
	return []interface{}{
		map[string]interface{}{"executionNodeIp": "10.0.0.1", "localPath": "/mnt/first"},
		map[string]interface{}{"executionNodeIp": "10.0.0.2", "localPath": "/data/second"},
	}
}

func TestTaskFieldsShowAllLocalParentBindings(t *testing.T) {
	for _, direction := range []string{"LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
		t.Run(direction, func(t *testing.T) {
			source := direction == "LOCAL_TO_OSS"
			key := "destinationEndpoint"
			if source {
				key = "sourceEndpoint"
			}
			task := map[string]interface{}{
				"taskIndex": 42, "parentTaskId": 42, "direction": direction,
				key: map[string]interface{}{"type": "LOCAL", "nodeBindings": taskFieldBindings()},
			}
			if got := taskEndpoint(task, source); got != "10.0.0.1:/mnt/first, 10.0.0.2:/data/second" {
				t.Fatalf("endpoint: %q", got)
			}
			if got := taskWorkers(task); got != "10.0.0.1,10.0.0.2" {
				t.Fatalf("workers: %q", got)
			}
		})
	}
}

func TestTaskFieldsPreferChildExecutionNodeAndMatchingBinding(t *testing.T) {
	task := map[string]interface{}{
		"taskIndex": 7, "parentTaskId": 42, "shardId": "local-1", "direction": "LOCAL_TO_OSS",
		"executionNodeIp": "10.0.0.2", "workerNode": "10.0.0.2",
		"eligibleWorkerNodes": []interface{}{"10.0.0.1", "10.0.0.2"},
		"sourceEndpoint":      map[string]interface{}{"type": "LOCAL", "nodeBindings": taskFieldBindings()},
	}
	if got := taskWorkers(task); got != "10.0.0.2" {
		t.Fatalf("child workers: %q", got)
	}
	if got := taskEndpoint(task, true); got != "/data/second" {
		t.Fatalf("child endpoint: %q", got)
	}
	task["localPath"] = "/actual/child-file"
	if got := taskEndpoint(task, true); got != "/actual/child-file" {
		t.Fatalf("actual child endpoint: %q", got)
	}
}

func TestTaskFieldsFallBackToTopLevelBindingsAndOssEndpoint(t *testing.T) {
	task := map[string]interface{}{
		"direction": "OSS_TO_LOCAL", "nodeBindings": taskFieldBindings(),
		"sourceEndpoint": map[string]interface{}{"type": "OSS", "s3Alias": "remote", "bucket": "archive"},
	}
	if got := taskEndpoint(task, true); got != "remote/archive" {
		t.Fatalf("OSS endpoint: %q", got)
	}
	if got := taskEndpoint(task, false); got != "10.0.0.1:/mnt/first, 10.0.0.2:/data/second" {
		t.Fatalf("local endpoint: %q", got)
	}
	task["nodeBindings"] = nil
	if got := taskEndpoint(task, false); got != "本地文件系统" {
		t.Fatalf("missing local endpoint: %q", got)
	}
}

func TestTaskFieldsParentWorkersUseCompleteMembership(t *testing.T) {
	task := map[string]interface{}{
		"taskIndex": 42, "parentTaskId": 42,
		"executionNodeIp":          "10.0.0.1",
		"participatingWorkerNodes": []interface{}{"10.0.0.1", "10.0.0.2"},
	}
	if got := taskWorkers(task); got != "10.0.0.1,10.0.0.2" {
		t.Fatalf("parent workers: %q", got)
	}
}

func TestTaskShowSelectsChildByActualNode(t *testing.T) {
	client := creationTestClient(t,
		creationExchange{method: "GET", path: "/status", response: `{"tasks":[{"taskIndex":42,"parentTaskId":42,"direction":"LOCAL_TO_OSS","eligibleWorkerNodes":["10.0.0.1","10.0.0.2"]}]}`},
		creationExchange{method: "GET", path: "/local-transfers/status", response: `{"success":true,"data":{"tasks":[],"shards":[{"taskIndex":9,"parentTaskId":42,"shardId":"local-1","shardIndex":1,"direction":"LOCAL_TO_OSS","executionNodeIp":"10.0.0.2","eligibleWorkerNodes":["10.0.0.1","10.0.0.2"],"sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/first"},{"executionNodeIp":"10.0.0.2","localPath":"/second"}]}}]}}`},
	)
	result, err := taskShow(client, []string{"42", "--child", "42-2", "--node", "10.0.0.2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	child, ok := result["child"].(map[string]interface{})
	if !ok || taskEndpoint(child, true) != "/second" || taskWorkers(child) != "10.0.0.2" {
		t.Fatalf("child result: %#v", result)
	}
}
