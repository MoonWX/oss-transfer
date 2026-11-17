package master

import (
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// Right after a retry the task row still counts the previous run's failed
// objects, which cannot be retried in place. failedChildCount counts only
// the child tasks of the current run that ended with failures.
func TestTaskRowCountsFailedChildrenOfTheCurrentRun(t *testing.T) {
	g := newGenerationReport(t, "a", "b")
	g.failures.set("a", "b")
	g.assign([]string{"a"}, []string{"b"})
	for number := 1; number <= 2; number++ {
		if record := g.run(number); record.Status != execution.Failed {
			t.Fatal("fixture child did not fail", number, record.Status)
		}
	}
	g.finish("FAILED")
	if row := g.f.master.taskView(g.refresh()); row["failedChildCount"] != 2 {
		t.Fatalf("ended task: failedChildCount %v", row["failedChildCount"])
	}
	g.resume()
	row := g.f.master.taskView(g.refresh())
	if row["status"] != "RUNNING" || row["failedObjectCount"] == int64(0) || row["failedChildCount"] != 0 {
		t.Fatalf("resumed task: status %v failedObjectCount %v failedChildCount %v", row["status"], row["failedObjectCount"], row["failedChildCount"])
	}
	if record := g.run(1); record.Status != execution.Failed {
		t.Fatal("retried child did not fail again", record.Status)
	}
	if row := g.f.master.taskView(g.refresh()); row["failedChildCount"] != 1 {
		t.Fatalf("a failed child of the current run: failedChildCount %v", row["failedChildCount"])
	}
	if child := g.f.master.childView(g.parent, g.child(1)); child["failedChildCount"] != nil {
		t.Fatalf("child row: %v", child["failedChildCount"])
	}
}
