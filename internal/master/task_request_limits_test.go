package master

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestTaskLimitsExportImportAndReportPreserveScope(t *testing.T) {
	for _, scope := range []string{"", "PER_NODE"} {
		request := ossTaskRequest()
		request.RequestLimits = &execution.RequestLimits{Scope: scope, ListQPS: 100, HeadQPS: 2800, PutQPS: 1400, BandwidthBitsPerSecond: 2e9}
		spec, _, _, err := normalizeTaskRequest(request, "add-buckets")
		if err != nil {
			t.Fatal(err)
		}
		expectedScope := scope
		if expectedScope == "" {
			expectedScope = "CLUSTER"
		}
		if spec.RequestLimits.Scope != expectedScope || request.RequestLimits.Scope != scope {
			t.Fatal("scope not normalized or request mutated")
		}
		spec.Nodes = []task.Node{{Address: "node.example"}}
		parent := task.Parent{ID: 7, Spec: spec, Status: "COMPLETED"}
		raw, _ := json.Marshal([]any{taskDTO(parent)})
		response := httptest.NewRecorder()
		rows, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(raw)))
		if !ok || len(rows) != 1 {
			t.Fatal("export could not reimport", response.Body.String())
		}
		if !reflect.DeepEqual(rows[0].RequestLimits, spec.RequestLimits) {
			t.Fatal("reimport lost limits")
		}
		var output bytes.Buffer
		writer := csv.NewWriter(&output)
		if err := writer.Write(reportSummaryHeader); err != nil {
			t.Fatal(err)
		}
		if err := summaryCSV(writer, parent, nil); err != nil {
			t.Fatal(err)
		}
		if err := legacySummaryCSV(writer, parent, task.LegacyChild{Number: 1, Raw: json.RawMessage(`{"status":"COMPLETED"}`)}); err != nil {
			t.Fatal(err)
		}
		writer.Flush()
		csvRows, err := csv.NewReader(&output).ReadAll()
		if err != nil {
			t.Fatal("summary columns misaligned", err)
		}
		for _, row := range csvRows[1:] {
			if row[len(row)-5] != expectedScope || row[len(row)-4] != "100" || row[len(row)-1] != "2000000000" {
				t.Fatal("report silently changed request caps", row)
			}
		}
	}
}

func TestTaskRejectsInvalidRequestLimits(t *testing.T) {
	for _, limits := range []*execution.RequestLimits{
		{Scope: "unknown", ListQPS: 1}, {Scope: "CLUSTER", HeadQPS: -1}, {PutQPS: 1e-12}, {BandwidthBitsPerSecond: 1},
	} {
		request := ossTaskRequest()
		request.RequestLimits = limits
		if _, _, _, err := normalizeTaskRequest(request, "add-buckets"); err == nil {
			t.Fatal("invalid request limits accepted", limits)
		}
	}
}
