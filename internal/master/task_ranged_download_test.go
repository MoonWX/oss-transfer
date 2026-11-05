package master

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func downloadTaskRequest(ranged string) taskCreateRequest {
	return taskCreateRequest{Direction: "OSS_TO_LOCAL", SourceEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"}, DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{"node.example", "/data"}}}, RangedDownload: ranged}
}

func TestRangedDownloadRequestNormalization(t *testing.T) {
	for value, want := range map[string]string{"": "", "auto": "", "AUTO": "", "off": "off", "Off": "off"} {
		spec, _, _, err := normalizeTaskRequest(downloadTaskRequest(value), "local-transfers")
		if err != nil || spec.RangedDownload != want {
			t.Errorf("rangedDownload %q: stored %q (%v), want %q", value, spec.RangedDownload, err, want)
		}
	}
	if _, _, _, err := normalizeTaskRequest(downloadTaskRequest("on"), "local-transfers"); err == nil {
		t.Error("unknown rangedDownload accepted")
	}
	// The default keeps the stored spec, and so the idempotency fingerprint,
	// identical to requests made before the option existed.
	plain, _, _, _ := normalizeTaskRequest(downloadTaskRequest(""), "local-transfers")
	auto, _, _, _ := normalizeTaskRequest(downloadTaskRequest("auto"), "local-transfers")
	encodedPlain, _ := json.Marshal(plain)
	encodedAuto, _ := json.Marshal(auto)
	if !bytes.Equal(encodedPlain, encodedAuto) || bytes.Contains(encodedPlain, []byte("rangedDownload")) {
		t.Errorf("automatic choice changed the stored spec: %s", encodedAuto)
	}
	oss := ossTaskRequest()
	oss.RangedDownload = "auto"
	if _, _, _, err := normalizeTaskRequest(oss, "add-buckets"); err != nil {
		t.Errorf("auto rejected for an OSS destination: %v", err)
	}
	// Normalization, which both /local-transfers/validate and creation use,
	// rejects "off" for every direction that does not write a local file.
	upload := taskCreateRequest{Direction: "LOCAL_TO_OSS", SourceEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{"node.example", "/data"}}}, DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "target"}, RangedDownload: "off"}
	oss.RangedDownload = "off"
	for name, row := range map[string]taskCreateRequest{"upload": upload, "OSS to OSS": oss} {
		if _, _, _, err := normalizeTaskRequest(row, "local-transfers"); err == nil || err.Error() != rangedDownloadOffLocalOnly {
			t.Errorf("%s with off: %v", name, err)
		}
	}
	for _, message := range []string{rangedDownloadOffLocalOnly, "rangedDownload must be auto or off", "ranged download can only be turned off for a LOCAL destination"} {
		for _, locale := range []string{"zh-CN", "ja-JP"} {
			if localizeMessage(locale, message) == message {
				t.Errorf("%q has no %s translation", message, locale)
			}
		}
	}
}

func TestRangedDownloadIsReportedAndSurvivesExportImport(t *testing.T) {
	for stored, want := range map[string]string{"": "auto", execution.RangedDownloadOff: "off"} {
		spec := task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "LOCAL"}, Nodes: []task.Node{{ID: "n", EnrollmentID: "e", Address: "node.example", LocalRoot: "/data"}}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", RangedDownload: stored}
		row := taskDTO(task.Parent{ID: 7, Spec: spec})
		if row["rangedDownload"] != want {
			t.Fatalf("detail reports %v, want %q", row["rangedDownload"], want)
		}
		row["nodeBindings"] = nil // an import supplies its own bindings
		encoded, _ := json.Marshal([]any{row})
		response := httptest.NewRecorder()
		rows, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(encoded)))
		if !ok || len(rows) != 1 || rows[0].RangedDownload != want {
			t.Fatalf("exported row did not import its choice: %v %s", rows, response.Body.String())
		}
	}
}
