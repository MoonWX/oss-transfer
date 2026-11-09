package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Command-line mistakes exit 2 before any request; failures reported by the
// Master exit 1; a rejected session is a failure, never a partial result.
func TestCLIExitCodesSeparateUsageFailuresAndIncompleteResults(t *testing.T) {
	noNetwork := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("usage error reached the server: %s %s", r.Method, r.URL)
	}
	for _, args := range [][]string{
		{"task", "show", "abc"},
		{"task", "create", "oss-to-oss", "seaweed/src-bucket"},
		{"task", "list", "--bogus"},
		{"update", "agents", "--yes"},
		{"bogus"},
		{"task"},
		{"task", "bogus"},
		{"storage", "remove", "source"},
		{"task", "purge", "7"},
		{"exceptions", "list", "nothing"},
		{"object", "status", "--limit", "0", "job-1"},
		{"--json", "task", "show", "abc"},
	} {
		code, out, stderr := runMockCLI(t, args, noNetwork)
		if code != 2 || stderr == "" {
			t.Errorf("%v: exit %d, want 2: %s %s", args, code, out, stderr)
		}
	}
	// The Master refused the request: an operation failure.
	code, _, stderr := runMockCLI(t, []string{"storage", "remove", "source", "--yes"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"success":false,"message":"Storage connection is used by a task"}`)
	})
	if code != 1 || !strings.Contains(stderr, "Storage connection is used by a task（HTTP 409）") {
		t.Fatalf("server rejection: %d %s", code, stderr)
	}
	// A session that must change its initial password is told how, and the
	// refused list is not reported as a partial result.
	for _, language := range []struct{ locale, message, hint string }{
		{"zh", "请先修改初始密码", "请先运行 oss-transfer-cli change-password 修改初始密码"},
		{"en", "Change the initial password before continuing", "run oss-transfer-cli change-password to change the initial password first"},
		{"ja", "初期パスワードを変更してください", "oss-transfer-cli change-password で初期パスワードを変更してください"},
	} {
		for _, args := range [][]string{{"task", "list"}, {"task", "list", "--all"}, {"node", "list"}} {
			code, out, stderr := runMockCLI(t, append([]string{"--lang", language.locale}, args...), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"success":false,"message":"`+language.message+`"}`)
			})
			if code != 1 || !strings.Contains(stderr, language.hint) || strings.Contains(out+stderr, "pageError") || strings.Contains(stderr, "部分") || strings.Contains(stderr, "incomplete") {
				t.Errorf("%s %v: %d %q %q", language.locale, args, code, out, stderr)
			}
			// The error is printed once, not repeated as a "message" field.
			if out != "" || !strings.Contains(stderr, language.message) {
				t.Errorf("%s %v: message repeated: %q %q", language.locale, args, out, stderr)
			}
		}
	}
	code, _, stderr = runMockCLI(t, []string{"whoami"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"success":false,"message":"需要登录"}`)
	})
	if code != 1 || !strings.Contains(stderr, "oss-transfer-cli login") {
		t.Fatalf("sign-in hint: %d %s", code, stderr)
	}
}

// With no Master given, configured or installed here, a refused connection
// to the default address says how to configure one.
func TestCLIDefaultMasterConnectionRefusedExplainsConfiguration(t *testing.T) {
	discoveryTestPaths(t)
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	original := http.DefaultTransport
	http.DefaultTransport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	run := func(args ...string) (int, string) {
		var out, errout bytes.Buffer
		code := runCLI(args, strings.NewReader(""), &out, &errout)
		return code, errout.String()
	}
	if host, fallback, err := discoverMaster(); err != nil || !fallback || host != defaultMasterOrigin {
		t.Fatal(host, fallback, err)
	}
	code, stderr := run("task", "list")
	if code != 1 || !strings.Contains(stderr, "config set master") || !strings.Contains(stderr, "--host") {
		t.Fatalf("%d %s", code, stderr)
	}
	// An explicit address gets the general hint naming that address.
	code, stderr = run("--host", "http://10.0.0.10:1685", "task", "list")
	if code != 1 || strings.Contains(stderr, "未找到本机安装") || !strings.Contains(stderr, "无法连接 Master（http://10.0.0.10:1685）") {
		t.Fatalf("%d %s", code, stderr)
	}
	// config get master names the fallback as such.
	var out bytes.Buffer
	if code := runCLI([]string{"config", "get", "master"}, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.Contains(out.String(), defaultMasterOrigin) || !strings.Contains(out.String(), "默认地址") || strings.Contains(out.String(), "source:") {
		t.Fatalf("%d %s", code, out.String())
	}
	out.Reset()
	if code := runCLI([]string{"--json", "config", "get", "master"}, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.Contains(out.String(), `"source":"automatic"`) {
		t.Fatalf("JSON source changed: %d %s", code, out.String())
	}
}

// Human output never prints numbers in scientific notation; times are local
// with their offset, unset times are omitted and booleans are words.
func TestCLIGenericOutputFormatsNumbersAndTimes(t *testing.T) {
	observed := time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC)
	result := map[string]interface{}{
		"objectSize": 5.24288e+06, "configuredLimitBps": 2e+09, "ratio": 0.25, "observedAt": float64(observed.UnixMilli()),
		"startedAt": "0001-01-01T00:00:00Z", "updatedAt": observed.Format(time.RFC3339), "readOnly": false, "success": true,
		"taskIds": []interface{}{float64(1234567)}, "revision": float64(12345678901),
	}
	var out bytes.Buffer
	renderCLIResult(&cliLocaleWriter{Writer: &out, locale: localeEN}, result)
	text := out.String()
	local := observed.Local().Format(cliTimeLayout)
	for _, want := range []string{"objectSize: 5242880", "configuredLimitBps: 2000000000", "ratio: 0.25", "observedAt: " + local, "updatedAt: " + local, "No", "1234567", "revision: 12345678901"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	for _, avoid := range []string{"e+", "0001-01-01", "startedAt", "Success", "true"} {
		if strings.Contains(text, avoid) {
			t.Errorf("contains %q:\n%s", avoid, text)
		}
	}
	for number, want := range map[float64]string{0: "0", 5242880: "5242880", 1794239815299: "1794239815299", 2e9: "2000000000", 1.5: "1.5", -3: "-3", 1e21: "1000000000000000000000"} {
		if got := cliNumberText(number); got != want {
			t.Errorf("%v: %s, want %s", number, got, want)
		}
	}
}

func exceptionsFixture() map[string]interface{} {
	row := func(key string, size float64, message, retry string, count float64) map[string]interface{} {
		return map[string]interface{}{
			"taskIndex": 1, "parentTaskId": 1, "childTaskId": "1-2", "direction": "OSS_TO_OSS", "key": key, "logicalKey": key,
			"objectSize": size, "workerNode": "10.0.0.2", "executionNodeIp": "10.0.0.2", "executionNodeLabel": "agent-a (10.0.0.2)",
			"stage": "TRANSFER", "errorCode": "TRANSFER_FAILED", "reasonCode": "TRANSFER_FAILED", "errorMessage": message,
			"retryCount": count, "retryStatus": retry, "timestamp": 1.794239815299e+12, "observedAt": 1.794239815299e+12,
			"timestampFormatted": "2026-11-10T00:00:00Z", "targetCommitted": false,
		}
	}
	return map[string]interface{}{
		"success": true, "failedKeys": map[string]interface{}{
			"10.0.0.2": []interface{}{
				row("photos/a.jpg", 5.24288e+06, "operation error S3: PutObject, https response error StatusCode: 403, RequestID: 1A2B, api error AccessDenied: Access Denied.\nsecond line", "NOT_RETRIED", 0),
				row("photos/b.jpg", 1024, "Get \"https://bucket.oss.example/b.jpg\": dial tcp: lookup bucket.oss.example: no such host", "FAILED", 2),
			},
			"10.0.0.2Count": float64(2),
		},
		"totalCount": 2, "queriedWorkers": []interface{}{"10.0.0.2"}, "failedWorkers": []interface{}{}, "unreachableWorkers": []interface{}{},
		"complete": true, "partial": false, "truncated": false, "workerErrors": map[string]interface{}{},
		"nodeDisplays": map[string]interface{}{"10.0.0.2": map[string]interface{}{"displayLabel": "agent-a (10.0.0.2)", "displayName": "agent-a", "displayAddress": "10.0.0.2"}},
	}
}

// exceptions list shows a table: one line per object with a readable size,
// the node, a short reason, the retry result and the time.
func TestExceptionsListRendersTableAndKeepsJSON(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/failed" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		adminTestJSON(t, w, exceptionsFixture())
	}
	observed := time.UnixMilli(1794239815299).Local().Format(cliTimeLayout)
	for _, test := range []struct {
		locale string
		wants  []string
	}{
		{"zh", []string{"任务", "大小", "原因", "重试", "#1-2", "photos/a.jpg", "5 MiB", "agent-a (10.0.0.2)", "存储服务拒绝访问", "未重试", "photos/b.jpg", "1 KiB", "无法解析存储服务地址", "再次失败（2 次）", observed, "共 2 条", "exceptions export"}},
		{"en", []string{"Task", "Size", "Reason", "Retry", "Access denied", "Not retried", "Failed again (2 times)", "Cannot resolve the storage address", "2 records"}},
		{"ja", []string{"タスク", "サイズ", "理由", "アクセスが拒否されました", "未再試行", "再失敗（2 回）"}},
	} {
		code, out, stderr := runMockCLI(t, []string{"--lang", test.locale, "exceptions", "list", "failed", "--task-id", "1"}, handler)
		if code != 0 || stderr != "" {
			t.Fatalf("%s: %d %s %s", test.locale, code, out, stderr)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", test.locale, want, out)
			}
		}
		for _, avoid := range []string{"e+06", "e+12", "childTaskId", "reasonCode", "destS3Alias", "second line", "RequestID"} {
			if strings.Contains(out, avoid) {
				t.Errorf("%s contains %q:\n%s", test.locale, avoid, out)
			}
		}
		// One line per object between the header and the totals.
		if lines := strings.Count(out, "photos/"); lines != 2 {
			t.Errorf("%s: %d object lines:\n%s", test.locale, lines, out)
		}
	}
	code, out, _ := runMockCLI(t, []string{"--json", "exceptions", "list", "failed", "--task-id", "1"}, handler)
	var result map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &result) != nil {
		t.Fatal(code, out)
	}
	items := objectList(result["items"])
	if len(items) != 2 || items[0]["errorMessage"] == nil || items[0]["objectSize"] != float64(5242880) || items[0]["retryStatus"] != "NOT_RETRIED" || result[cliSummaryKey] != nil {
		t.Fatalf("JSON changed: %s", out)
	}
	// Nothing failed.
	empty := exceptionsFixture()
	empty["failedKeys"] = map[string]interface{}{"10.0.0.2": []interface{}{}, "10.0.0.2Count": float64(0)}
	code, out, _ = runMockCLI(t, []string{"exceptions", "list", "failed"}, func(w http.ResponseWriter, r *http.Request) { adminTestJSON(t, w, empty) })
	if code != 0 || !strings.Contains(out, "没有失败对象") || strings.Contains(out, "exceptions export") {
		t.Fatal(code, out)
	}
}

// A task create whose only row is refused created nothing: a definite
// failure with the reason, not an uncertain partial submission.
func TestTaskCreateRejectedRowsAreDefiniteFailures(t *testing.T) {
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/master/local-nodes" {
				io.WriteString(w, `{"success":true,"data":{"nodes":[{"executionNodeIp":"10.0.0.1","reachable":true}]}}`)
				return
			}
			io.WriteString(w, body)
		}
	}
	rejected := reply(`{"success":true,"partial":true,"taskIds":[],"rejectedCount":1,"validationErrors":["1: storage connection nosuchconn does not exist"],"addedCount":0}`)
	for _, test := range []struct{ locale, want string }{
		{"zh", "未创建任务：存储连接 nosuchconn 不存在"},
		{"en", "No task was created: Storage connection nosuchconn does not exist"},
		{"ja", "タスクは作成されませんでした: ストレージ接続 nosuchconn は存在しません"},
	} {
		code, out, stderr := runMockCLI(t, []string{"--lang", test.locale, "task", "create", "oss-to-oss", "nosuchconn/x", "target/dst-bucket"}, rejected)
		if code != 1 || !strings.Contains(stderr, test.want) || strings.Contains(out+stderr, "部分任务可能") || strings.Contains(out+stderr, "request-id") || strings.Contains(out+stderr, "Go ") {
			t.Errorf("%s: %d %q %q", test.locale, code, out, stderr)
		}
	}
	code, out, _ := runMockCLI(t, []string{"--json", "task", "create", "oss-to-oss", "nosuchconn/x", "target/dst-bucket"}, rejected)
	var result map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &result) != nil || result["success"] != false || result["submissionStatus"] != "rejected" || result["partial"] != false || result["rejectedCount"] != float64(1) {
		t.Fatalf("%d %s", code, out)
	}
	// Two rows, one refused: the created task is named, the other reason
	// shown, and the result is partial.
	file := filepath.Join(t.TempDir(), "rows.json")
	if err := os.WriteFile(file, []byte(`[{"sourceS3Alias":"source","sourceBucket":"a","destS3Alias":"target","destBucket":"b"},{"sourceS3Alias":"nosuchconn","sourceBucket":"c","destS3Alias":"target","destBucket":"d"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	mixed := reply(`{"success":true,"partial":true,"taskIds":[5],"rejectedCount":1,"validationErrors":["2: storage connection nosuchconn does not exist"],"addedCount":1}`)
	code, out, stderr := runMockCLI(t, []string{"task", "import", "buckets", file}, mixed)
	if code != 3 || !strings.Contains(out, "已创建 1 个任务：#5") || !strings.Contains(stderr, "1 个任务未创建：第 2 项：存储连接 nosuchconn 不存在") || strings.Contains(out+stderr, "部分任务可能") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	// A reply whose counts do not add up stays uncertain and keeps the
	// request ID for a safe retry.
	unknown := reply(`{"success":true,"partial":true,"taskIds":[5],"unknownCount":1,"addedCount":1}`)
	code, _, stderr = runMockCLI(t, []string{"task", "import", "buckets", file}, unknown)
	if code != 3 || !strings.Contains(stderr, "--request-id") || !strings.Contains(stderr, "部分任务可能已受理") {
		t.Fatalf("%d %q", code, stderr)
	}
	// An accepted task names its ID instead of printing raw fields.
	code, out, _ = runMockCLI(t, []string{"--lang", "en", "task", "create", "oss-to-oss", "source/x", "target/y"}, reply(`{"success":true,"partial":false,"taskIds":[12],"rejectedCount":0,"validationErrors":[],"addedCount":1}`))
	if code != 0 || !strings.Contains(out, "Created 1 task: #12") || !strings.Contains(out, "task show 12") || strings.Contains(out, "addedCount") || strings.Contains(out, "submissionStatus") {
		t.Fatalf("%d %s", code, out)
	}
}

// Write commands print one confirmation instead of their raw reply.
func TestWriteCommandsPrintSummaries(t *testing.T) {
	t.Setenv("CLI_TEST_AK", "access")
	t.Setenv("CLI_TEST_SK", "secret")
	nodes := `{"success":true,"data":{"nodes":[{"address":"10.0.0.2","label":"edge","agent":true,"online":true}]}}`
	for _, test := range []struct {
		name         string
		args         []string
		reply        func(*http.Request) string
		wants, avoid []string
	}{
		{"storage add", []string{"storage", "add", "source", "--endpoint", "http://10.0.0.9:8333", "--access-key-env", "CLI_TEST_AK", "--secret-key-env", "CLI_TEST_SK", "--validate", "false"},
			func(*http.Request) string {
				return `{"success":true,"message":"存储连接已保存","revision":3,"successCount":2,"failedCount":0,"partial":false,"details":["a: 同步成功","b: 同步成功"]}`
			},
			[]string{"存储连接 source 已保存，已同步到 2 个节点"}, []string{"revision", "successCount", "failedCount", "成功:"}},
		{"storage update", []string{"storage", "update", "source", "--validate=false", "--region", "us-east-1"},
			func(r *http.Request) string {
				if r.Method == http.MethodGet {
					return `{"success":true,"configs":[{"alias":"source","endpoint":"http://10.0.0.9:8333"}]}`
				}
				return `{"success":true,"successCount":1,"failedCount":1,"partial":true,"details":["a: 同步成功","b: 等待同步"]}`
			},
			[]string{"存储连接 source 已保存，已同步到 1 个节点，1 个节点等待同步", "b: 等待同步"}, []string{"revision"}},
		{"node update", []string{"node", "update", "10.0.0.2", "--label", "edge"}, func(*http.Request) string { return nodes },
			[]string{"节点 10.0.0.2 已更新：名称：edge"}, []string{"连接状态"}},
		{"agent update", []string{"agent", "update", "10.0.0.2", "--child-concurrency", "8"}, func(*http.Request) string { return nodes },
			[]string{"节点 10.0.0.2 已更新：子任务并发：8"}, []string{"连接状态"}},
		{"whoami", []string{"whoami"}, func(*http.Request) string {
			return `{"success":true,"user":{"username":"admin","displayName":"admin","role":"admin","mustChangePassword":false}}`
		}, []string{"用户名", "admin", "角色", "管理员"}, []string{"节点名称", "revision"}},
		{"exceptions clear", []string{"exceptions", "clear", "failed", "--all", "--yes"}, func(r *http.Request) string {
			if strings.HasSuffix(r.URL.Path, "/clear") {
				return `{"success":true,"clearedCount":3,"complete":true,"partial":false,"queriedWorkers":[],"failedWorkers":[],"unreachableWorkers":[],"scope":"resolved-exceptions","executionLedgerPreserved":true}`
			}
			return `{"success":true,"clearableCount":3,"failedKeys":{}}`
		}, []string{"已清理 3 条已处理的记录"}, []string{"请求上限范围", "resolved-exceptions"}},
		{"update agents", []string{"update", "agents", "--all", "--version", "4.1.41", "--yes"}, func(*http.Request) string {
			return `{"success":true,"data":{"id":"q1","version":"4.1.41","phase":"RUNNING","items":[{"nodeId":"n1","address":"10.0.0.2","phase":"QUEUED","updatedAt":"2026-11-10T00:00:00Z","startedAt":"0001-01-01T00:00:00Z"}]}}`
		}, []string{"已安排把 1 个 Agent 更新到 v4.1.41", "10.0.0.2", "排队中", "update status"}, []string{"0001-01-01", "startedAt", "QUEUED"}},
		{"task resume", []string{"task", "resume", "7"}, func(r *http.Request) string {
			if r.Method == http.MethodGet {
				return `{"success":true,"data":{"taskIndex":7,"status":"STOPPED","generation":2}}`
			}
			return `{"success":true,"accepted":1,"failed":0,"ignored":0,"partial":false,"details":[{"taskId":7,"status":"RESUMING","generation":3,"success":true,"accepted":true,"ignored":false}]}`
		}, []string{"#7", "已恢复执行"}, []string{"batches", "已受理:", "taskIds"}},
	} {
		code, out, stderr := runMockCLI(t, test.args, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, test.reply(r)) })
		if code != 0 && !(test.name == "storage update" && code == 3) {
			t.Errorf("%s: exit %d: %s %s", test.name, code, out, stderr)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", test.name, want, out)
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out, avoid) {
				t.Errorf("%s contains %q:\n%s", test.name, avoid, out)
			}
		}
		// --json keeps the reply.
		code, out, _ = runMockCLI(t, append([]string{"--json"}, test.args...), func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, test.reply(r)) })
		var result map[string]interface{}
		if json.Unmarshal([]byte(out), &result) != nil || result[cliSummaryKey] != nil || strings.Contains(out, cliSummaryKey) {
			t.Errorf("%s JSON: %d %s", test.name, code, out)
		}
	}
}

// task report --output - writes the verified ZIP to standard output and
// nothing else.
func TestTaskReportToStandardOutput(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, _ := writer.Create("summary.csv")
	io.WriteString(entry, "rowType\n")
	writer.Close()
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/task-reports/7" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(archive.Bytes())
	}
	code, out, stderr := runMockCLI(t, []string{"task", "report", "7", "--output", "-"}, handler)
	if code != 0 || stderr != "" || out != archive.String() {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	if entries, err := os.ReadDir("."); err == nil {
		for _, entry := range entries {
			if entry.Name() == "-" {
				t.Fatal("report saved to a file named -")
			}
		}
	}
	code, _, stderr = runMockCLI(t, []string{"--json", "task", "report", "7", "--output", "-"}, func(http.ResponseWriter, *http.Request) { t.Error("request with --json") })
	if code != 2 || !strings.Contains(stderr, "--json") {
		t.Fatalf("%d %s", code, stderr)
	}
	// A saved report is confirmed in one line.
	path := filepath.Join(t.TempDir(), "report.zip")
	code, out, _ = runMockCLI(t, []string{"task", "report", "7", "--output", path}, handler)
	if code != 0 || !strings.Contains(out, "任务 #7 的报告已保存到 "+path) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLIVersionShowsVPrefix(t *testing.T) {
	oldVersion, oldCommit := version, commit
	version, commit = "4.2.0", "abc1234"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
	var out bytes.Buffer
	if code := runCLI([]string{"--lang", "en", "--version"}, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.HasPrefix(out.String(), "OSS Transfer CLI v4.2.0\n") || !strings.Contains(out.String(), "Commit abc1234") {
		t.Fatalf("%d %s", code, out.String())
	}
	out.Reset()
	if code := runCLI([]string{"--version", "--json"}, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.Contains(out.String(), `"version":"4.2.0"`) {
		t.Fatalf("JSON version changed: %s", out.String())
	}
}

func TestCLIHintsWhenTheMasterCannotBeReached(t *testing.T) {
	clearLocaleEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := "http://" + listener.Addr().String()
	listener.Close()
	for _, test := range []struct{ lang, want string }{
		{"zh", "提示: 无法连接 Master（" + host + "）。请检查地址和端口"},
		{"en", "Hint: could not connect to the Master (" + host + "). Check that the address and port"},
		{"ja", "ヒント: Master（" + host + "）に接続できません。"},
	} {
		var out, stderr bytes.Buffer
		if code := runCLI([]string{"--host", host, "--lang", test.lang, "task", "list"}, strings.NewReader(""), &out, &stderr); code != 1 {
			t.Fatalf("%s: exit %d: %s", test.lang, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), test.want) || !strings.Contains(stderr.String(), "config set master") {
			t.Fatalf("%s: %s", test.lang, stderr.String())
		}
	}
	// An HTTP error from a reachable Master gets no connection hint.
	if hints := cliErrorHints(&apiHTTPError{Status: 500, Message: "boom"}, false); len(hints) != 0 {
		t.Fatal(hints)
	}
}
