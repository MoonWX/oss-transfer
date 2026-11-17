package master

import (
	"slices"
	"testing"
	"time"
)

// A completed prefix comparison that found no object on either side compared
// nothing. Its summary stays complete, but warns: "complete" alone is not a
// finding that every object matched.
func TestComparisonExportWarnsWhenNothingWasCompared(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-empty", agent)
	empty := completedComparison("cmp-empty", nil)
	empty.Compared, empty.Matched = 0, 0
	agent.put(empty, nil)
	rows := fakeComparisonRows(3)
	agent.put(completedComparison("cmp-found", rows), rows)
	for _, id := range []string{"cmp-empty", "cmp-found"} {
		putComparisonAssignment(t, f, node, comparisonTestRequest(id), time.Now())
	}
	for id, warnings := range map[string][]string{"cmp-empty": {"NO_OBJECTS_FOUND"}, "cmp-found": {}} {
		_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/"+id+"/export", f.admin, ""))
		summary := exportSummary(t, files["summary.json"])
		if !summary.Complete || !slices.Equal(summary.Warnings, warnings) {
			t.Fatalf("%s: %+v", id, summary)
		}
	}
}
