package main

import (
	"net/http"
	"strings"
	"testing"
)

// The conclusion under a comparison's counts follows the counts: "no
// differences" only when objects were checked and all matched. A comparison
// that checked nothing found no objects, and differences that were counted
// but not stored are still differences.
func TestComparisonConclusionFollowsTheCounts(t *testing.T) {
	none := map[string]interface{}{"compared": 0, "matched": 0, "different": 0, "etagMismatch": 0, "missing": 0, "extra": 0, "changed": 0, "errors": 0, "bothMissing": 0, "rows": 0}
	unsaved := map[string]interface{}{"compared": 5000, "matched": 0, "different": 0, "etagMismatch": 0, "missing": 5000, "extra": 0, "changed": 0, "errors": 0, "bothMissing": 0, "rows": 0, "rowsTruncated": true, "truncatedReason": "LOW_DISK_SPACE"}
	for _, test := range []struct {
		name         string
		locale       string
		page         map[string]interface{}
		wants, avoid []string
	}{
		{"nothing checked", "zh", comparisonTestPage(none), []string{"没有找到任何对象，请检查两端的存储桶和前缀（Key 区分大小写，通常不以 / 开头）"}, []string{"未发现差异", "本页没有差异记录"}},
		{"nothing checked", "en", comparisonTestPage(none), []string{"No object was found. Check the bucket and prefix on both sides"}, []string{"No differences found"}},
		{"nothing checked", "ja", comparisonTestPage(none), []string{"オブジェクトが見つかりませんでした。"}, []string{"見つかりませんでした\n差"}},
		{"unsaved differences", "zh", comparisonTestPage(unsaved), []string{"目标缺失", "5000", "节点磁盘剩余空间不足", "发现 5000 条差异，但节点未保存明细，原因见上方“截断原因”"}, []string{"未发现差异"}},
		{"unsaved differences", "en", comparisonTestPage(unsaved), []string{"Found 5000 differences, but the node did not store their details; see the truncation reason above"}, []string{"No differences found"}},
		{"unsaved differences", "ja", comparisonTestPage(unsaved), []string{"5000 件の差分が見つかりましたが、ノードは詳細を保存していません。"}, []string{"差分は見つかりませんでした"}},
		{"interrupted", "zh", comparisonTestPage(map[string]interface{}{"status": "INTERRUPTED", "compared": 10, "matched": 10, "different": 0, "etagMismatch": 0, "missing": 0, "extra": 0, "changed": 0, "errors": 0, "bothMissing": 0, "rows": 0}), []string{"本页没有差异记录"}, []string{"未发现差异"}},
		{"one object", "zh", comparisonTestPage(map[string]interface{}{"scope": "object", "compared": 1, "matched": 0, "different": 1, "etagMismatch": 0, "missing": 0, "extra": 0, "changed": 0, "errors": 0, "bothMissing": 0, "rows": 0}), []string{"发现 1 条差异，但没有保存明细"}, []string{"未发现差异"}},
	} {
		code, out, _ := runMockCLI(t, []string{"--lang", test.locale, "object", "status", "job-1"}, func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, test.page) })
		if code != 0 && code != 3 {
			t.Fatalf("%s %s: exit %d: %s", test.name, test.locale, code, out)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s %s lacks %q:\n%s", test.name, test.locale, want, out)
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out, avoid) {
				t.Errorf("%s %s says %q:\n%s", test.name, test.locale, avoid, out)
			}
		}
	}
}
