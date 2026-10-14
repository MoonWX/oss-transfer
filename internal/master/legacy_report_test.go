package master

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/legacyimport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// This fixture never starts the task scheduler. Migration guards are tested by
// legacyimport; here a synthetic finalized catalogue exercises the HTTP reader.
func legacyReportFixture(t *testing.T) *agentHTTPFixture {
	t.Helper()
	f := newAgentHTTPFixture(t)
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put("migration", "state", map[string]any{"migrationId": "report-fixture", "pending": true})
	}); err != nil {
		t.Fatal(err)
	}
	parent := task.Parent{ID: 224, Generation: 1, BudgetRevision: 1, Status: "COMPLETED", ScanComplete: true, CreatedAt: time.Unix(1700000000, 0).UTC(), ScannedObjects: 3, ScannedBytes: 12, ChildCount: 1, FinishedChildren: 1, Counters: execution.Counters{Total: 3, TotalBytes: 12, Succeeded: 1, SucceededBytes: 3, Skipped: 1, SkippedBytes: 4, Failed: 1, FailedBytes: 5}, Legacy: &task.LegacyProvenance{MigrationID: "report-fixture", OriginalStatus: "COMPLETED", SourceNodes: []string{"old-node"}}, Spec: task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "old", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "old", Bucket: "destination"}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}}
	parent.Status = "PARTIAL_FAILED"
	parent.Legacy.NeedsReconciliation = true
	children := []task.LegacyChild{{ParentID: 224, Number: 1, Node: "old-node", TaskID: -100, Key: "task:-100", Raw: json.RawMessage(`{"taskIndex":-100,"parentTaskId":224,"direction":"OSS_TO_OSS","status":"PARTIAL_FAILED","sourceS3Alias":"old","sourceBucket":"source","destS3Alias":"old","destBucket":"destination","putObjectCount":1,"scanComplete":true,"marker":"mid-key","createTime":1700000000000}`)}}
	if err := f.master.tasks.repo.ImportLegacy(task.LegacyImport{MigrationID: "report-fixture", Parents: []task.LegacyParentImport{{Parent: parent, Children: children}}}); err != nil {
		t.Fatal(err)
	}
	rows := []task.LegacyHistory{{ParentID: 224, Node: "old-node", Key: "file-report:224:coverage", Kind: "COVERAGE", Cursor: "coverage", Raw: json.RawMessage(`{"ledgerCoverage":"TRACKED","scanComplete":true,"discoveredCount":3}`)}}
	for index, result := range []string{"SUCCESS", "SKIPPED", "FAILED"} {
		raw, _ := json.Marshal(map[string]any{"logicalKey": "=object-" + strconv.Itoa(index), "sourceKey": "source/" + strconv.Itoa(index), "destinationKey": "target/" + strconv.Itoa(index), "result": result, "size": index + 3, "completedAt": int64(1700000000000), "sourceS3Alias": "old", "destinationS3Alias": "old", "sourceBucket": "source", "destinationBucket": "destination"})
		rows = append(rows, task.LegacyHistory{ParentID: 224, ChildTaskID: "224-1", Node: "old-node", Key: "file-report:224:item:" + strconv.Itoa(index), Kind: "OBJECT", Cursor: strconv.Itoa(index), Raw: raw})
	}
	if err := f.master.tasks.repo.PutLegacyHistory("report-fixture", rows); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestLegacyReportAndChildDetailsRetainFactsWithoutLiveAgents(t *testing.T) {
	f := legacyReportFixture(t)
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/tasks/224", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	detail := body["data"].(map[string]any)
	if detail["legacyHistorical"] != true || detail["contentVerified"] != false || detail["migrationRecoveryRequired"] != true {
		t.Fatal(detail)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/tasks/224/children/1", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatal(items)
	}
	child := items[0].(map[string]any)
	if child["childTaskId"] != "224-1" || child["legacyHistorical"] != true || child["marker"] != "mid-key" || child["taskIndex"] != float64(224) {
		t.Fatal(child)
	}
	status, files := fetchReport(t, f, http.MethodGet, "/api/master/task-reports/224", f.admin, "")
	if status != 200 {
		t.Fatal(status)
	}
	csvRows, err := csv.NewReader(strings.NewReader(string(files["file-results.csv"]))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(csvRows) != 4 {
		t.Fatal(csvRows)
	}
	columns := map[string]int{}
	for i, name := range csvRows[0] {
		columns[strings.TrimPrefix(name, "\ufeff")] = i
	}
	if _, ok := columns["taskId"]; ok {
		t.Fatal("internal task ID leaked")
	}
	results := map[string]bool{}
	for _, row := range csvRows[1:] {
		results[row[columns["result"]]] = true
		if row[columns["childTaskId"]] != "224-1" || row[columns["targetCommitted"]] != "" || row[columns["ledgerObservedAt"]] != "" || !strings.HasPrefix(row[columns["logicalKey"]], "'=object-") {
			t.Fatal(row)
		}
	}
	if !results["SUCCESS"] || !results["SKIPPED"] || !results["FAILED"] {
		t.Fatal(results)
	}
	var metadata map[string]any
	if json.Unmarshal(files["report-metadata.json"], &metadata) != nil {
		t.Fatal("metadata")
	}
	if metadata["contentVerified"] != false || metadata["containsLegacyHistory"] != true || metadata["countsReconciled"] != true || len(metadata["unreachableNodes"].([]any)) != 0 {
		t.Fatal(metadata)
	}
	if metadata["complete"] != false {
		t.Fatal("unfinished recovery report marked complete")
	}
}

func TestRealV4ReportConversion(t *testing.T) {
	source := os.Getenv("OSS_TRANSFER_LEGACY_TASK_SNAPSHOT")
	if source == "" {
		t.Skip("set OSS_TRANSFER_LEGACY_TASK_SNAPSHOT to a real stopped V4 capture")
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := legacyimport.Inspect(context.Background(), input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := legacyimport.Prepare(context.Background(), legacyimport.PrepareOptions{SnapshotPaths: []string{source}, Destination: filepath.Join(t.TempDir(), "destination"), PublicURL: "http://127.0.0.1:1685", DirectNodes: map[string]legacyimport.DirectNodeApproval{plan.Header.Node: {Address: plan.Header.Node}}})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := prepared.ConvertTasks(context.Background(), legacyimport.TaskImportOptions{LegacyTimezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	// Exercise the production report writer without releasing the startup fence.
	s := &Server{tasks: &taskRuntime{repo: task.New(prepared.Master)}, agents: &agentControl{registry: cluster.New(prepared.Master, nil), lastSeen: map[string]time.Time{}}, options: Options{Now: time.Now}}
	parents, err := s.tasks.repo.List(0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	temporary, err := os.CreateTemp(t.TempDir(), "exceptions-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer temporary.Close()
	if err := s.writeTaskReport(context.Background(), &output, temporary, parents); err != nil {
		t.Fatal(err)
	}
	zipped, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	countByParent := map[string]int{}
	successByParent := map[string]int{}
	var metadata map[string]any
	for _, entry := range zipped.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == "file-results.csv" {
			rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			header := map[string]int{}
			for index, name := range rows[0] {
				header[strings.TrimPrefix(name, "\ufeff")] = index
			}
			for _, row := range rows[1:] {
				id := row[header["parentTaskId"]]
				countByParent[id]++
				if row[header["result"]] == "SUCCESS" {
					successByParent[id]++
				}
				child := row[header["childTaskId"]]
				if child != "" && !strings.HasPrefix(child, id+"-") {
					t.Fatal("incorrect public child mapping", child)
				}
				if row[header["targetCommitted"]] != "" {
					t.Fatal("fabricated current commitment")
				}
			}
		}
		if entry.Name == "report-metadata.json" {
			if json.Unmarshal(raw, &metadata) != nil {
				t.Fatal("invalid metadata")
			}
		}
	}
	for _, id := range []string{"1", "2", "5", "6"} {
		if countByParent[id] != 260 || successByParent[id] != 260 {
			t.Fatal("completed report differs from genuine V4 capture", id, countByParent, successByParent)
		}
	}
	if successByParent["7"] != 3 || successByParent["8"] != 1 {
		t.Fatal("partial success history changed", successByParent)
	}
	if metadata["containsLegacyHistory"] != true || metadata["contentVerified"] != false {
		t.Fatal(metadata)
	}
	if err := prepared.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("report export released migration guard", err)
	}
	t.Logf("real V4 report rows by parent: %v; historical successes: %v", countByParent, successByParent)
}
