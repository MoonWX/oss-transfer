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
		for _, direction := range []string{"LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
			var buffer bytes.Buffer
			out := &cliLocaleWriter{Writer: &buffer, locale: locale}
			renderTaskDetail(out, map[string]interface{}{"taskIndex": float64(1), "direction": direction, "status": "COMPLETED", "sharedCheckSkipped": true})
			renderTaskReportResult(out, map[string]interface{}{"reportKind": "task", "complete": true, "resultCounts": map[string]int64{}, "sharedCheckSkipped": true})
			text := buffer.String()
			want := []string{"共享目录校验", "此任务创建时跳过了共享目录校验", "此报告中的任务创建时跳过了共享目录校验"}
			if locale == localeEN {
				want = []string{"Shared directory check", "Warning: this task was created with the shared directory check skipped", "Warning: a task in this report was created with the shared directory check skipped"}
			}
			if direction == "OSS_TO_LOCAL" {
				// A download's consequence is a split destination.
				want = append(want, map[cliLocale]string{localeZH: "分散在各节点各自的目录中", localeEN: "split across each node's own directory"}[locale])
			}
			for _, phrase := range want {
				if !strings.Contains(text, phrase) {
					t.Fatalf("%v %s output lacks %q:\n%s", locale, direction, phrase, text)
				}
			}
		}
	}
}

func TestDownloadSkipStatesItsOwnConsequences(t *testing.T) {
	var warnings bytes.Buffer
	client := creationTestClient(t)
	client.errors = &warnings
	_, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--binding", "10.0.0.2=/mnt/data", "--skip-shared-check"})
	if err == nil || !strings.Contains(warnings.String(), "下载的文件会分散写入各节点各自的目录") || strings.Contains(warnings.String(), "上传") {
		t.Fatal("a download skip did not state a download's consequences", err, warnings.String())
	}
}

func TestImportWarnsAboutEveryRowThatSkipsTheCheck(t *testing.T) {
	upload := `"direction":"LOCAL_TO_OSS","sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data"},{"executionNodeIp":"10.0.0.2","localPath":"/mnt/data"}]},"destinationEndpoint":{"type":"OSS","s3Alias":"target","bucket":"dst"}`
	file := creationImportFile(t, "input.json", `[{`+upload+`},{`+upload+`,"skipSharedCheck":true,"skipSharedCheckAcknowledged":true},{`+upload+`,"skipSharedCheck":true,"skipSharedCheckAcknowledged":true}]`)
	var warnings bytes.Buffer
	client := creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":false,"nodeBindings":[]}}`})
	client.errors = &warnings
	_, _ = runTaskImport(client, []string{"local", file, "--validate-only"})
	if !strings.Contains(warnings.String(), "第 2, 3 项跳过了共享目录校验") || !strings.Contains(warnings.String(), "下载的文件会分散") {
		t.Fatal("skipped rows were imported without a warning", warnings.String())
	}
}

func TestTaskNodesNeedsItsOwnAcknowledgementForASkippedTask(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	master.view["sharedCheckSkipped"] = true
	var warnings bytes.Buffer
	client := adminTestClient(t, master.handler(t))
	client.errors = &warnings
	args := []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share", "--yes"}
	if _, err := runTaskCommand(client, args); err == nil || !isCLIUsageError(err) || len(master.posts) != 0 || !strings.Contains(warnings.String(), "新加入的节点同样不会校验") {
		t.Fatal("a node joined a skipped task without its own acknowledgement", err, master.posts, warnings.String())
	}
	if _, err := runTaskCommand(client, append(args, "--skip-shared-check")); err != nil {
		t.Fatal(err)
	}
	if len(master.posts) != 2 || master.posts[0]["dryRun"] != true || master.posts[1]["skipSharedCheckAcknowledged"] != true {
		t.Fatal("the acknowledgement was not sent", master.posts)
	}
	// The flag means nothing for a task that checks its directories.
	plain := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	if _, err := runTaskCommand(adminTestClient(t, plain.handler(t)), append(args, "--skip-shared-check")); err == nil || !isCLIUsageError(err) || len(plain.posts) != 0 {
		t.Fatal("--skip-shared-check was accepted for a checked task", err, plain.posts)
	}
	var buffer bytes.Buffer
	renderTaskNodeBindingChecks(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, []map[string]interface{}{{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share", "valid": true, "code": "SHARED_STORAGE_CHECK_SKIPPED", "message": "未校验：此任务创建时跳过了共享目录校验"}})
	if text := buffer.String(); !strings.Contains(text, "未校验") || strings.Contains(text, "通过") {
		t.Fatal("an unchecked node is shown as passed", text)
	}
}
