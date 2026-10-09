package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func cliTrashFixture() map[string]interface{} {
	return map[string]interface{}{
		"success": true, "totalCount": 3, "complete": true,
		"queriedWorkers":    []string{"10.0.0.1", "10.0.0.2"},
		"successfulWorkers": []string{"10.0.0.1", "10.0.0.2"},
		"deletedTasks": map[string]interface{}{
			"10.0.0.2": []interface{}{map[string]interface{}{"taskIndex": 224, "direction": "OSS_TO_OSS", "status": "COMPLETED", "sourceBucket": "photos", "destBucket": "archive", "sourceS3Alias": "source", "destS3Alias": "target", "putObjectCount": 7, "listObjectCount": 7, "skippedObjectCount": 0, "failedObjectCount": 0}},
			"10.0.0.1": []interface{}{
				map[string]interface{}{"taskIndex": 226, "direction": "LOCAL_TO_OSS", "status": "STOPPED", "sourceEndpoint": map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.2", "localPath": "/data/files"}}}, "destinationEndpoint": map[string]interface{}{"type": "OSS", "s3Alias": "target", "bucket": "uploads"}},
				map[string]interface{}{"taskIndex": 225, "direction": "OSS_TO_LOCAL", "status": "FAILED", "sourceEndpoint": map[string]interface{}{"type": "OSS", "s3Alias": "source", "bucket": "photos"}, "destinationEndpoint": map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.2", "localPath": "/data/downloads"}}}},
			},
		},
	}
}

func TestCLITrashReadableRowsRetainOriginalEnvelope(t *testing.T) {
	result := cliTrashFixture()
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/master/deleted-tasks" {
			t.Fatalf("wrong recycle query: %s %s", r.Method, r.URL)
		}
		adminTestJSON(t, w, result)
	})
	queried, err := taskTrash(client, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := objectList(queried["recycledTasks"])
	if len(rows) != 3 || getInt(rows[0], "taskIndex") != 226 || getInt(rows[2], "taskIndex") != 224 {
		t.Fatalf("unstable parent order: %#v", rows)
	}
	if queried["totalCount"] != float64(3) || queried["complete"] != true || len(queried["successfulWorkers"].([]interface{})) != 2 {
		t.Fatalf("lost original snapshot: %#v", queried)
	}
	groups := queried["deletedTasks"].(map[string]interface{})
	if _, changed := objectList(groups["10.0.0.1"])[0]["recycleNode"]; changed {
		t.Fatal("display normalization changed original grouped JSON")
	}
	var output bytes.Buffer
	renderCLIResult(&output, queried)
	for _, want := range []string{"#224", "#225", "#226", "source/photos", "target/uploads", "/data/downloads", "显示 3 条回收站记录"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "deletedTasks:") {
		t.Fatal("recycle table exposed raw nested records")
	}
}

func TestCLITrashFiltersDirectionsNodeBindingsAndIDs(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []int
	}{
		{[]string{"--task-id", "#225"}, []int{225}},
		{[]string{"--direction", "local-to-oss"}, []int{226}},
		{[]string{"--worker", "10.0.0.2"}, []int{226, 225, 224}},
		{[]string{"--direction", "oss-to-local", "--worker", "10.0.0.2"}, []int{225}},
		{[]string{"--search", "UPLOADS"}, []int{226}},
		{[]string{"--search", "missing"}, []int{}},
	} {
		client := adminTestClient(t, func(w http.ResponseWriter, _ *http.Request) { adminTestJSON(t, w, cliTrashFixture()) })
		result, err := taskTrash(client, test.args)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		rows := objectList(result["recycledTasks"])
		if len(rows) != len(test.want) {
			t.Fatalf("%v: %#v", test.args, rows)
		}
		for i, want := range test.want {
			if getInt(rows[i], "taskIndex") != want {
				t.Fatalf("%v: %#v", test.args, rows)
			}
		}
	}
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid filter reached server") })
	for _, args := range [][]string{{"224"}, {"--worker", "not-ip"}, {"--task-id", "0"}, {"--direction", "unknown"}, {"--search", "a", "--search", "b"}} {
		if _, err := taskTrash(client, args); err == nil {
			t.Fatalf("invalid recycle filter accepted: %v", args)
		}
	}
}

func TestCLITrashPartialAndMalformedSnapshotsRemainIncomplete(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		code, output, stderr := runMockCLI(t, []string{"task", "trash", "--json"}, func(w http.ResponseWriter, _ *http.Request) {
			result := cliTrashFixture()
			result["complete"] = false
			if malformed {
				result["deletedTasks"].(map[string]interface{})["10.0.0.2"] = 0
			} else {
				result["failedWorkers"] = []string{"10.0.0.2"}
			}
			adminTestJSON(t, w, result)
		})
		var result map[string]interface{}
		if code != 3 || json.Unmarshal([]byte(output), &result) != nil || result["complete"] != false || stderr == "" {
			t.Fatalf("partial recycle claimed complete: %d %s %s", code, output, stderr)
		}
		if malformed && !strings.Contains(stderr, "未返回有效") {
			t.Fatalf("malformed group silently ignored: %s", stderr)
		}
	}
}
