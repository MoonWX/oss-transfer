package main

import (
	"bytes"
	"strings"
	"testing"
)

// An Agent from before the write probe passes a download destination without
// checking that it can write there. The CLI must not report a plain pass.
func TestCreateDownloadSaysWhenWritingWasNotChecked(t *testing.T) {
	const unchecked = `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true,"pathType":"DIRECTORY","code":"LOCAL_PATH_WRITE_NOT_CHECKED","message":"执行节点版本较旧，未检查目录是否可写；升级该节点后重新预检即可检查"}]}}`
	client := creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: unchecked})
	result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--validate-only"})
	if err != nil || result["submissionStatus"] != "validated" {
		t.Fatalf("%#v %v", result, err)
	}
	if message := getString(result, "message"); message != "路径预检通过，但执行节点 10.0.0.1 版本较旧，未检查目录是否可写；尚未创建迁移任务" {
		t.Fatalf("validated message %q", message)
	}

	var warnings bytes.Buffer
	client = creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: unchecked},
		creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[42]}}`})
	client.errors = &warnings
	if result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data"}); err != nil || result["submissionStatus"] != "accepted" {
		t.Fatalf("%#v %v", result, err)
	}
	if !strings.Contains(warnings.String(), "警告：执行节点 10.0.0.1 版本较旧，未检查下载目录是否可写") {
		t.Fatalf("warnings %q", warnings.String())
	}

	// A destination the Agent proved writable passes as before.
	client = creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true,"pathType":"DIRECTORY","message":"目标目录可写，可用空间 1.0 TiB"}]}}`})
	result, err = runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--validate-only"})
	if err != nil || getString(result, "message") != "节点预检通过，尚未创建迁移任务" {
		t.Fatalf("%#v %v", result, err)
	}
}
