package master

import (
	"fmt"
	"net/http"
	"testing"
)

// One online Agent runs the connection test and the validation on save, while
// a task may run on every node. Each result names that node, so a pass or a
// failure is read as what it is: the result of that node.
func TestStorageChecksNameTheNodeThatRanThem(t *testing.T) {
	f := newAgentHTTPFixture(t)
	s3 := newStorageCheckS3(t)
	node := comparisonTestNode(t, f, "storage-checker", &storageCheckAgent{s3: s3})
	if code, body := request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+node.Address, f.admin, map[string]any{"label": "Office LAN"}); code != http.StatusOK {
		t.Fatal("rename", code, body)
	}
	label := f.master.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
	if label != "Office LAN (127.0.0.1)" {
		t.Fatal("display label", label)
	}
	url := f.http.URL + "/api/master/s3-config"

	code, body := request(t, f.http.Client(), "POST", url, f.admin, storageCheckConfig("http://storage.example.com:9000", nil))
	if code != http.StatusOK || body["validationNode"] != label {
		t.Fatalf("validated save: HTTP %d %v", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets", f.admin, nil)
	if code != http.StatusOK || body["checkedBucket"] != "src" || body["node"] != label {
		t.Fatalf("connection test: HTTP %d %v", code, body)
	}

	s3.set("AccessDenied", "NoSuchBucket")
	code, body = request(t, f.http.Client(), "GET", url+"/selfhosted/buckets?bucket=src", f.admin, nil)
	if code != http.StatusBadGateway || body["reasonCode"] != "STORAGE_NO_SUCH_BUCKET" || body["node"] != label {
		t.Fatalf("failed connection test: HTTP %d %v", code, body)
	}
	s3.set("InvalidAccessKeyId", "")
	config := storageCheckConfig("http://storage.example.com:9000", nil)
	config["alias"] = "second"
	code, body = request(t, f.http.Client(), "POST", url, f.admin, config)
	if code != http.StatusBadRequest || body["reasonCode"] != "STORAGE_INVALID_ACCESS_KEY" || body["node"] != label {
		t.Fatalf("failed validation: HTTP %d %v", code, body)
	}
	// Without an online node no node ran the check, and none is named. The
	// only registered node is offline, so the message asks to bring it back.
	f.master.agents.mu.Lock()
	delete(f.master.agents.lastSeen, node.ID)
	f.master.agents.mu.Unlock()
	code, body = request(t, f.http.Client(), "POST", url, f.admin, config)
	if code != http.StatusBadRequest || body["node"] != nil || fmt.Sprint(body["message"]) != "Storage validation failed: every registered Agent is offline; bring one back online before validating storage" {
		t.Fatalf("validation without a node: HTTP %d %v", code, body)
	}
}
