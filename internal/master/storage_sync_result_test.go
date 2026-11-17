package master

import (
	"net/http"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// Saving or deleting a connection while nodes are offline does not fail: the
// Master has the change, and a node that missed it is synced before it next
// works. The result names the action and says when the others sync. Offline
// nodes are not waited for, so they cannot keep the connected node from
// syncing within the request.
func TestStorageSaveAndDeleteNameTheActionAndWhenOfflineNodesSync(t *testing.T) {
	f := newAgentHTTPFixture(t)
	offline := joinOfflineAgents(t, f, 9)
	agent := nodeTestAgent(t, f, "z-live")
	connections := func() []storageconfig.Config {
		t.Helper()
		snapshot, err := agent.service.Configs.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.Connections
	}
	check := func(body map[string]any, message, note string) {
		t.Helper()
		if body["success"] != true || body["message"] != message || body["syncNote"] != note ||
			body["successCount"] != float64(1) || body["failedCount"] != float64(len(offline)) || body["partial"] != true {
			t.Fatal("storage result", body)
		}
		details, _ := body["details"].([]any)
		synced := 0
		for _, raw := range details {
			detail := raw.(string)
			switch {
			case strings.HasSuffix(detail, ": 同步成功") || strings.HasSuffix(detail, ": Synced"):
				synced++
			case !strings.HasSuffix(detail, ": 等待同步") && !strings.HasSuffix(detail, ": Awaiting sync"):
				t.Fatal("unexpected node detail", detail)
			}
		}
		if len(details) != len(offline)+1 || synced != 1 {
			t.Fatal("node details", details)
		}
	}

	code, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, "zh-CN",
		map[string]any{"alias": "archive", "endpoint": "http://127.0.0.1:9", "accessKey": "fixture-ak", "secretKey": "fixture-sk", "validate": false})
	if err != nil || code != http.StatusOK {
		t.Fatal(code, body, err)
	}
	check(body, "存储连接已保存", "未同步的节点会在下次执行任务或对象比对前自动同步。")
	if got := connections(); len(got) != 1 || got[0].Alias != "archive" {
		t.Fatal("the connected node did not receive the connection", got)
	}

	code, _, body, err = localizedRequest(f.http.Client(), "DELETE", f.http.URL+"/api/master/s3-config/archive", f.admin, "en-US", nil)
	if err != nil || code != http.StatusOK {
		t.Fatal(code, body, err)
	}
	check(body, "Storage connection deleted", "Nodes that have not synced yet sync automatically before they next run a task or an object comparison; until then they keep this connection and its credentials.")
	if got := connections(); len(got) != 0 {
		t.Fatal("the connected node kept the deleted connection", got)
	}
}
