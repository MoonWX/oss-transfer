package master

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func resolvedCSVRow(t *testing.T, header []string, write func(*csv.Writer) error) map[string]string {
	t.Helper()
	var buffer bytes.Buffer
	w := csv.NewWriter(&buffer)
	if err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	if err := write(w); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(buffer.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatal("expected one report row", rows, err)
	}
	result := map[string]string{}
	for i, key := range rows[0] {
		result[key] = rows[1][i]
	}
	return result
}

func TestResolvedInventorySizeMatchesSummariesFilesAndChildDetail(t *testing.T) {
	known := execution.Item{ID: "known", Source: transferstore.Object{Key: "known", Size: 9, ETag: "known"}, DestinationKey: "copied/known"}
	missing := execution.Item{ID: "missing", Source: transferstore.Object{Key: "supplied-later"}, DestinationKey: "copied/later"}
	pin := transferstore.Object{Key: "supplied-later", Size: 123, ETag: "first-observed"}
	counts := execution.Counters{Total: 2, TotalBytes: 132, Succeeded: 2, SucceededBytes: 132}
	p := task.Parent{ID: 17, Status: "COMPLETED", ScannedObjects: 2, ScannedBytes: 9, DispatchedObjects: 2, DispatchedBytes: 9, Counters: counts,
		Spec: task.Spec{InventoryID: "inventory", Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "storage", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "storage", Bucket: "target"}}}
	job := execution.Job{ParentID: 17, ChildID: "17-1", Number: 1, Items: []execution.Item{known, missing}, Source: p.Spec.Source, Destination: p.Spec.Destination, DiscoveryComplete: true}
	resolved := execution.ItemResult{ID: missing.ID, Status: execution.ItemSucceeded, ResolvedSource: &pin}
	child := task.Child{Job: job, Node: task.Node{Address: "agent"}, Record: &execution.Record{Job: job, InventorySource: true, Status: execution.Completed, Counters: counts,
		Results: []execution.ItemResult{{ID: known.ID, Status: execution.ItemSucceeded}, resolved}}}
	for _, entry := range []struct {
		name  string
		child *task.Child
	}{{"parent", nil}, {"child", &child}} {
		t.Run(entry.name+"_summary", func(t *testing.T) {
			row := resolvedCSVRow(t, reportSummaryHeader, func(w *csv.Writer) error { return summaryCSV(w, p, entry.child) })
			for key, want := range map[string]string{"scannedCount": "2", "scannedBytes": "132", "successCount": "2", "successBytes": "132"} {
				if row[key] != want {
					t.Errorf("%s = %q, want %q", key, row[key], want)
				}
			}
		})
	}
	for _, status := range []string{"SUCCESS", "FAILED", "UNKNOWN"} {
		t.Run("file_"+status, func(t *testing.T) {
			var issues bytes.Buffer
			errorsWriter := csv.NewWriter(&issues)
			row := resolvedCSVRow(t, reportFileHeader, func(w *csv.Writer) error {
				return fileCSV(w, errorsWriter, p, child, missing, resolved, status, "fixture", time.Time{})
			})
			if row["size"] != "123" || row["logicalKey"] != "supplied-later" || row["childTaskId"] != "17-1" {
				t.Fatal("file report lost the resolved source", row)
			}
			errorsWriter.Flush()
			if status != "SUCCESS" {
				issue, err := csv.NewReader(&issues).Read()
				if err != nil || issue[4] != "123" {
					t.Fatal("exception report lost the resolved size", issue, err)
				}
			}
		})
	}
	row := childTaskDTO(p, child)
	for key, want := range map[string]string{"listObjectCount": "2", "listObjectSize": "132", "putObjectCount": "2", "putObjectSize": "132"} {
		if fmt.Sprint(row[key]) != want {
			t.Errorf("child detail %s = %v, want %s", key, row[key], want)
		}
	}
	if missing.Source.Size != 0 || child.Job.Items[1].Source.Size != 0 {
		t.Fatal("presentation rewrote the immutable missing-key assignment")
	}
}
