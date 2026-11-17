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
// Nothing was left out of the result, so the generic notice of an incomplete
// result is not printed; the exit status 3 marks the unsynced nodes, as the
// storage help says.
func TestStorageChangesSayWhenUnsyncedNodesSync(t *testing.T) {
	note := "未同步的节点会在下次执行任务或对象比对前自动同步；在此之前，这些节点上仍保留该连接及其凭据。"
	code, out, errout := runMockCLI(t, []string{"storage", "remove", "source", "--yes"}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/master/s3-config/source" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"success":true,"message":"存储连接已删除","revision":4,"successCount":1,"failedCount":1,"partial":true,"details":["a: 同步成功","b: 等待同步"],"syncNote":"`+note+`"}`)
	})
	if code != 3 || errout != "" {
		t.Fatalf("exit %d: %s\nstandard error: %s", code, out, errout)
	}
	for locale, want := range map[cliLocale]string{
		localeZH: "如有执行节点尚未同步，结果会列出这些节点，并以退出码 3 结束",
		localeEN: "nodes that have not synchronized yet are listed and the\ncommand exits with status 3",
		localeJA: "まだ同期していない実行ノードがあれば一覧に表示し、終了コード 3 で終了します",
	} {
		if help := helpForLocale([]string{"storage"}, locale); !strings.Contains(help, want) {
			t.Errorf("%s storage help lacks %q", locale, want)
		}
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
