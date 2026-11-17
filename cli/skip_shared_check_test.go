package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

const sharedPrecheckOK = `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true},{"executionNodeIp":"10.0.0.2","localPath":"/mnt/data","valid":true}]}}`

var sharedUpload = []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--binding", "10.0.0.2=/mnt/data"}

func TestCreateSkipSharedCheckRequiresYesAndWarns(t *testing.T) {
	var warnings bytes.Buffer
	client := creationTestClient(t)
	client.errors = &warnings
	_, err := runTaskCreate(client, append(append([]string(nil), sharedUpload...), "--skip-shared-check"))
	if err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("skip without --yes: %v", err)
	}
	if !strings.Contains(warnings.String(), "任务报告可能显示成功") {
		t.Fatal("the consequences were not printed", warnings.String())
	}

	warnings.Reset()
	check := func(rows []map[string]interface{}) {
		if rows[0]["skipSharedCheck"] != true || rows[0]["skipSharedCheckAcknowledged"] != true {
			t.Fatal("skip and acknowledgement were not sent", rows)
		}
	}
	client = creationTestClient(t,
		creationExchange{method: "POST", path: "/local-transfers/validate", response: sharedPrecheckOK, inspect: check},
		creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[42]}}`, inspect: check},
	)
	client.errors = &warnings
	if result, err := runTaskCreate(client, append(append([]string(nil), sharedUpload...), "--skip-shared-check", "--yes")); err != nil || result["submissionStatus"] != "accepted" {
		t.Fatalf("%#v %v", result, err)
	}
	if !strings.Contains(warnings.String(), "警告：--skip-shared-check") {
		t.Fatal("confirmed skip did not warn", warnings.String())
	}
}

func TestCreateSkipSharedCheckIsIgnoredForOneNodeAndRejectedElsewhere(t *testing.T) {
	var warnings bytes.Buffer
	check := func(rows []map[string]interface{}) {
		if _, present := rows[0]["skipSharedCheck"]; present {
			t.Fatal("single-node task sent the skip", rows)
		}
	}
	client := creationTestClient(t,
		creationExchange{method: "POST", path: "/local-transfers/validate", response: rangedDownloadPrecheckOK, inspect: check},
	)
	client.errors = &warnings
	if _, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--skip-shared-check", "--validate-only"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "已忽略 --skip-shared-check") {
		t.Fatal("ignored flag was not explained", warnings.String())
	}
	for _, args := range [][]string{
		{"oss-to-oss", "source/src", "target/dst", "--skip-shared-check", "--yes"},
		append(append([]string(nil), sharedUpload...), "--yes"),
	} {
		if _, err := runTaskCreate(creationTestClient(t), args); err == nil || !isCLIUsageError(err) {
			t.Fatalf("%v: want usage error, got %v", args, err)
		}
	}
	code, _, stderr := runMockCLI(t, append([]string{"task", "create"}, append(append([]string(nil), sharedUpload...), "--skip-shared-check")...), func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected network") })
	if code != 2 || !strings.Contains(stderr, "--skip-shared-check") || !strings.Contains(stderr, "--yes") {
		t.Fatalf("%d %s", code, stderr)
	}
}

func TestImportRequiresSkipSharedCheckAcknowledgement(t *testing.T) {
	upload := `"direction":"LOCAL_TO_OSS","sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data"},{"executionNodeIp":"10.0.0.2","localPath":"/mnt/data"}]},"destinationEndpoint":{"type":"OSS","s3Alias":"target","bucket":"dst"}`
	for _, data := range []string{
		`[{` + upload + `,"skipSharedCheck":true}]`,
		`[{` + upload + `,"skipSharedCheck":true,"skipSharedCheckAcknowledged":false}]`,
		`[{` + upload + `,"skipSharedCheck":"yes","skipSharedCheckAcknowledged":true}]`,
	} {
		file := creationImportFile(t, "input.json", data)
		if _, err := runTaskImport(creationTestClient(t), []string{"local", file}); err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "skipSharedCheck") {
			t.Fatalf("%s: want usage error, got %v", data, err)
		}
	}
}

func TestTaskDetailAndReportWarnAboutSkippedSharedCheck(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN} {
		var buffer bytes.Buffer
		out := &cliLocaleWriter{Writer: &buffer, locale: locale}
		renderTaskDetail(out, map[string]interface{}{"taskIndex": float64(1), "direction": "LOCAL_TO_OSS", "status": "COMPLETED", "sharedCheckSkipped": true})
		renderTaskReportResult(out, map[string]interface{}{"reportKind": "task", "complete": true, "resultCounts": map[string]int64{}, "sharedCheckSkipped": true})
		text := buffer.String()
		want := []string{"共享目录校验", "此任务创建时跳过了共享目录校验"}
		if locale == localeEN {
			want = []string{"Shared directory check", "created with the shared directory check skipped"}
		}
		for _, phrase := range want {
			if strings.Count(text, phrase) == 0 {
				t.Fatalf("%v output lacks %q:\n%s", locale, phrase, text)
			}
		}
		if strings.Count(text, want[1]) != 2 {
			t.Fatalf("detail and report must both warn:\n%s", text)
		}
	}
}
