package master

import (
	"strings"
	"testing"
)

// A task converted from v4.0.x keeps its failed count, but its failed objects
// are imported history that the exception index never holds. The exception
// APIs say so for that task instead of answering with a bare empty list.
func TestLegacyTaskExceptionListsPointToTheTaskReport(t *testing.T) {
	f := legacyReportFixture(t)
	failed := func(path, language string, body any) map[string]any {
		t.Helper()
		method := "POST"
		if body == nil {
			method = "GET"
		}
		status, _, result, err := localizedRequest(f.http.Client(), method, f.http.URL+path, f.admin, language, body)
		if err != nil || status != 200 {
			t.Fatal(path, status, result, err)
		}
		return result
	}
	result := failed("/api/master/failed", "zh-CN", map[string]any{"taskIndex": 224})
	if result["legacyHistorical"] != true || result["totalCount"] != float64(0) || !strings.Contains(result["message"].(string), "file-results.csv") || !strings.Contains(result["message"].(string), "v4.0.x 转换") {
		t.Fatalf("failed list of a converted task: %v", result)
	}
	local := failed("/api/master/local-transfers/failures?taskId=224", "en-US", nil)["data"].(map[string]any)
	if local["legacyHistorical"] != true || !strings.Contains(local["message"].(string), "converted from v4.0.x") {
		t.Fatalf("local failure list of a converted task: %v", local)
	}
	fallback := failed("/api/master/fallback", "zh-CN", map[string]any{"taskIndex": 224})
	if fallback["supported"] != false || fallback["legacyHistorical"] != true || !strings.Contains(fallback["message"].(string), "降级记录不在此列出") {
		t.Fatalf("fallback list of a converted task: %v", fallback)
	}
	// Without a converted task in the filter nothing changes.
	if all := failed("/api/master/failed", "zh-CN", map[string]any{}); all["legacyHistorical"] != nil || all["message"] != nil {
		t.Fatalf("an unfiltered list carried the converted-task notice: %v", all)
	}
}
