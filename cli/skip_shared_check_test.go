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
	// Nothing is skipped or recorded: nodes added later are checked, and the
	// hint says how to skip their check then.
	if text := warnings.String(); !strings.Contains(text, "已忽略 --skip-shared-check") || !strings.Contains(text, "之后用 task nodes 加入节点时会校验共享目录") || !strings.Contains(text, "--skip-shared-check 和 --yes") {
		t.Fatal("ignored flag was not explained", text)
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
			// A node change can record the skip as well as creation, and the
			// nodes added later are still checked.
			want := []string{"共享目录校验", "此任务在创建或加入节点时跳过了共享目录校验", "之后加入的节点仍会校验", "此报告中的任务在创建或加入节点时跳过了共享目录校验"}
			if locale == localeEN {
				want = []string{"Shared directory check", "Warning: this task skipped the shared directory check when it was created or when nodes were added", "Nodes added later are still checked", "Warning: a task in this report skipped the shared directory check"}
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

// Nodes added to a task that skipped the check earlier are checked: nothing
// is skipped unless the change asks for it, and no warning is printed.
func TestTaskNodesChecksNodesAddedToASkippedTask(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	master.view["sharedCheckSkipped"] = true
	var warnings bytes.Buffer
	client := adminTestClient(t, master.handler(t))
	client.errors = &warnings
	result, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share"})
	if err == nil || isCLIUsageError(err) || len(master.posts) != 1 || master.posts[0]["dryRun"] != true {
		t.Fatal("an unconfirmed change must preflight and stop", err, master.posts)
	}
	if _, present := master.posts[0]["skipSharedCheck"]; present || warnings.Len() != 0 {
		t.Fatal("a change that skips nothing asked to skip or warned", master.posts[0], warnings.String())
	}
	if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "校验新节点（此任务此前跳过了校验）") {
		t.Fatal("the plan does not say that the new node is checked", text)
	}
	if text := renderTaskNodesForTest(result, localeEN); !strings.Contains(text, "The new nodes are checked (this task skipped the check earlier)") {
		t.Fatal("English plan", text)
	}
	if _, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share", "--yes"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range master.posts[1:] {
		if _, present := body["skipSharedCheck"]; present {
			t.Fatal("a change that skips nothing asked to skip", body)
		}
		if _, present := body["skipSharedCheckAcknowledged"]; present {
			t.Fatal("a change that skips nothing acknowledged a skip", body)
		}
	}
	// The membership view says what the earlier skip means for later nodes.
	view, err := runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(view, localeZH); !strings.Contains(text, "此任务跳过了共享目录校验；之后加入的节点仍会校验") {
		t.Fatal("membership output", text)
	}
}

// --skip-shared-check on a node change prints creation's consequences to
// standard error, needs --yes before anything is sent, and then sends the
// skip with its acknowledgement in the dry run and in the change.
func TestTaskNodesSkipSharedCheckWarnsNeedsYesAndSendsBothFields(t *testing.T) {
	for _, direction := range []string{"LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
		master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
		master.view["direction"] = direction
		var warnings bytes.Buffer
		client := adminTestClient(t, master.handler(t))
		client.errors = &warnings
		args := []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share", "--skip-shared-check"}
		_, err := runTaskCommand(client, args)
		if err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "--yes") || len(master.posts) != 0 {
			t.Fatalf("%s: a skip without --yes was sent: %v %v", direction, err, master.posts)
		}
		want, other := "它会从与其他节点不同的数据中读取对象", "它下载的文件会写入其他节点看不到的目录"
		if direction == "OSS_TO_LOCAL" {
			want, other = other, want
		}
		if text := warnings.String(); !strings.Contains(text, "警告：--skip-shared-check 会跳过新加入节点的共享目录校验") || !strings.Contains(text, want) || strings.Contains(text, other) || !strings.Contains(text, "节点失联后的自动接管也不做校验") {
			t.Fatalf("%s: consequences:\n%s", direction, text)
		}
		result, err := runTaskCommand(client, append(args, "--yes"))
		if err != nil {
			t.Fatal(err)
		}
		if len(master.posts) != 2 || master.posts[0]["dryRun"] != true || master.posts[1]["dryRun"] != nil {
			t.Fatalf("%s: requests %v", direction, master.posts)
		}
		for _, body := range master.posts {
			if body["skipSharedCheck"] != true || body["skipSharedCheckAcknowledged"] != true {
				t.Fatalf("%s: the skip was not sent with its acknowledgement: %v", direction, body)
			}
		}
		if text := renderTaskNodesForTest(map[string]interface{}{"taskNodesPlan": map[string]interface{}{"taskId": 12, "add": []interface{}{"10.0.0.3"}, "skipSharedCheck": true}}, localeZH); !strings.Contains(text, "跳过，不验证新节点挂载的是同一份存储") {
			t.Fatalf("%s: plan %s", direction, text)
		}
		if result["requestId"] == nil {
			t.Fatal("the accepted change lost its request ID")
		}
	}
	var buffer bytes.Buffer
	renderTaskNodeBindingChecks(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, []map[string]interface{}{{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share", "valid": true, "code": "SHARED_STORAGE_CHECK_SKIPPED", "message": "已按要求跳过共享目录校验；未验证各节点是否挂载同一份存储"}})
	if text := buffer.String(); !strings.Contains(text, "未校验") || strings.Contains(text, "通过") {
		t.Fatal("an unchecked node is shown as passed", text)
	}
}

// --skip-shared-check needs nodes to add to a local transfer. A change whose
// additions only keep draining nodes adds no local path: the flag has no
// effect, says so, and is still sent so that a retry sends the same request.
func TestTaskNodesSkipSharedCheckNeedsLocalNodesToAdd(t *testing.T) {
	code, _, stderr := runMockCLI(t, []string{"task", "nodes", "12", "--remove", "10.0.0.2", "--skip-shared-check", "--yes"}, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected request") })
	if code != 2 || !strings.Contains(stderr, "--skip-shared-check") {
		t.Fatalf("%d %s", code, stderr)
	}
	plain := &taskNodesFakeMaster{view: taskNodesTestView(false)}
	if _, err := runTaskCommand(adminTestClient(t, plain.handler(t)), []string{"nodes", "12", "--add", "10.0.0.3", "--skip-shared-check", "--yes"}); err == nil || !isCLIUsageError(err) || len(plain.posts) != 0 {
		t.Fatal("--skip-shared-check was accepted for an OSS transfer", err, plain.posts)
	}
	master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
	var warnings bytes.Buffer
	client := adminTestClient(t, master.handler(t))
	client.errors = &warnings
	if _, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.2", "--skip-shared-check", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if text := warnings.String(); !strings.Contains(text, "本次没有新加入的节点") || strings.Contains(text, "警告") {
		t.Fatal("a change without new nodes", text)
	}
	if len(master.posts) != 1 || master.posts[0]["skipSharedCheck"] != true || master.posts[0]["dryRun"] != nil {
		t.Fatal("requests", master.posts)
	}
}
