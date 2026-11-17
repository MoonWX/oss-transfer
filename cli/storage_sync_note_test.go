package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A node that missed a connection change is synced before it next works; the
// Master says so, and for a deletion that the node keeps the credentials until
// then. The summary carries that note after the nodes still awaiting sync.
func TestStorageChangesSayWhenUnsyncedNodesSync(t *testing.T) {
	note := "未同步的节点会在下次执行任务或对象比对前自动同步；在此之前，这些节点上仍保留该连接及其凭据。"
	code, out, _ := runMockCLI(t, []string{"storage", "remove", "source", "--yes"}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/master/s3-config/source" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"success":true,"message":"存储连接已删除","revision":4,"successCount":1,"failedCount":1,"partial":true,"details":["a: 同步成功","b: 等待同步"],"syncNote":"`+note+`"}`)
	})
	if code != 3 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"存储连接 source 已删除，已同步到 1 个节点，1 个节点等待同步", "a: 同步成功", "b: 等待同步", note}
	if len(lines) != len(want) {
		t.Fatalf("summary lines:\n%s", out)
	}
	for i, line := range lines {
		if strings.TrimSpace(line) != want[i] {
			t.Fatalf("line %d is %q, want %q:\n%s", i, line, want[i], out)
		}
	}
}
