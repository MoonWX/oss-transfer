package master

import (
	"encoding/csv"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestExecutionTimeDTOAndReportExcludeManualWait(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	finish := start.Add(48*time.Hour + 90*time.Second)
	timing := &execution.ActiveTime{ElapsedMillis: 90000}
	p := task.Parent{ID: 1, Status: "PARTIAL_FAILED", StartedAt: &start, FinishedAt: &finish, ActiveTime: timing, Counters: execution.Counters{SucceededBytes: 900}, Spec: task.Spec{Direction: "OSS_TO_OSS"}}
	c := task.Child{Job: execution.Job{ParentID: 1, ChildID: "1-1", Number: 1}, Record: &execution.Record{Status: execution.PartialFailed, StartedAt: &start, FinishedAt: &finish, ActiveTime: timing, Counters: execution.Counters{SucceededBytes: 900}}}
	for _, entry := range []struct {
		name  string
		row   map[string]any
		child *task.Child
	}{{"parent", taskDTO(p), nil}, {"child", childTaskDTO(p, c), &c}} {
		t.Run(entry.name, func(t *testing.T) {
			if entry.row["duration"] != "1m30s" || entry.row["averageSpeedBytesPerSecond"] != float64(10) || entry.row["activeStartedAt"] != nil || entry.row["activeDurationMillis"] != int64(90000) {
				t.Fatal(entry.row)
			}
			row := resolvedCSVRow(t, reportSummaryHeader, func(w *csv.Writer) error { return summaryCSV(w, p, entry.child) })
			if row["executionDurationMillis"] != "90000" || row["startTime"] == "" || row["endTime"] == "" {
				t.Fatal(row)
			}
		})
	}
	for _, status := range []string{"RESUMING", "WAITING_RECOVERY", "STOPPED", "FAILED", "COMPLETED"} {
		p.Status, p.FinishedAt = status, nil
		row := taskDTO(p)
		if row["duration"] != "1m30s" || row["activeStartedAt"] != nil {
			t.Fatal(status, row)
		}
	}
	row := map[string]any{}
	current := finish.Add(time.Hour)
	clock := execution.ActiveTime{ElapsedMillis: 90000, StartedAt: &current}
	executionTimeDTO(row, clock, &start, nil, 1200, current.Add(30*time.Second))
	if row["duration"] != "2m0s" || row["averageSpeedBytesPerSecond"] != float64(10) || row["activeDurationMillis"] != int64(90000) || row["activeStartedAt"] != current.UnixMilli() {
		t.Fatal(row)
	}
	// A child with no attempt must not inherit its parent's accumulated timer.
	p.Status, p.ActiveTime = "RUNNING", &clock
	row = childTaskDTO(p, task.Child{Job: c.Job})
	for _, key := range []string{"duration", "activeStartedAt", "activeDurationMillis", "averageSpeedBytesPerSecond"} {
		if _, exists := row[key]; exists {
			t.Fatal("pending child inherited parent timing", key, row)
		}
	}
}
