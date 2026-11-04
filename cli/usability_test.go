package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func usabilityCreateHandler(t *testing.T, response string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/local-nodes":
			io.WriteString(w, `{"success":true,"data":{"nodes":[{"executionNodeIp":"10.0.0.1","reachable":true}]}}`)
		case "/api/master/add-buckets", "/api/master/import":
			io.WriteString(w, response)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
	}
}

func TestCLICreateSummaryNamesTaskAndNextStep(t *testing.T) {
	handler := usabilityCreateHandler(t, `{"success":true,"partial":false,"taskIds":[13],"rejectedCount":0,"validationErrors":[],"addedCount":1}`)
	for _, test := range []struct{ locale, created, next string }{
		{"en", "Created task #13 (oss-to-oss src/photos → dst/archive)", "Follow progress: oss-transfer-cli task show 13 --watch 5"},
		{"zh", "已创建任务 #13（oss-to-oss src/photos → dst/archive）", "查看进度: oss-transfer-cli task show 13 --watch 5"},
		{"ja", "タスク #13 を作成しました（oss-to-oss src/photos → dst/archive）", "進捗の確認: oss-transfer-cli task show 13 --watch 5"},
	} {
		code, out, stderr := runMockCLI(t, []string{"--lang", test.locale, "task", "create", "oss-to-oss", "src/photos", "dst/archive"}, handler)
		if code != 0 || stderr != "" || !strings.Contains(out, test.created) || !strings.Contains(out, test.next) {
			t.Fatalf("%s: %d %q %q", test.locale, code, out, stderr)
		}
		for _, internal := range []string{"addedCount", "rejectedCount", "validationErrors", "Request ID"} {
			if strings.Contains(out, internal) {
				t.Fatalf("%s: internal field %q in summary: %s", test.locale, internal, out)
			}
		}
	}
	// --json keeps the complete envelope.
	code, out, _ := runMockCLI(t, []string{"--json", "task", "create", "oss-to-oss", "src/photos", "dst/archive"}, handler)
	var envelope map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &envelope) != nil || envelope["submissionStatus"] != "accepted" || envelope["addedCount"] != float64(1) || envelope["requestId"] == nil {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLICreateRejectionIsADefiniteFailure(t *testing.T) {
	handler := usabilityCreateHandler(t, `{"success":true,"partial":true,"taskIds":[],"rejectedCount":1,"validationErrors":["1: destination bucket nosuch does not exist"],"addedCount":0}`)
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "task", "create", "oss-to-oss", "src/photos", "dst/nosuch"}, handler)
	if code != 1 || !strings.Contains(stderr, "no task was created: destination bucket nosuch does not exist") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	for _, misleading := range []string{"may have been accepted", "--request-id", "1: destination"} {
		if strings.Contains(stderr+out, misleading) {
			t.Fatalf("misleading %q: %s %s", misleading, out, stderr)
		}
	}
	code, out, _ = runMockCLI(t, []string{"--json", "task", "create", "oss-to-oss", "src/photos", "dst/nosuch"}, handler)
	var envelope map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &envelope) != nil || envelope["submissionStatus"] != "rejected" || envelope["success"] != false {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLICreateMixedBatchListsAcceptedAndRejectedRows(t *testing.T) {
	file := creationImportFile(t, "rows.json", `[{"sourceBucket":"a","destBucket":"b"},{"sourceBucket":"c","destBucket":"d"}]`)
	handler := usabilityCreateHandler(t, `{"success":true,"partial":true,"taskIds":[21],"rejectedCount":1,"validationErrors":["2: destination bucket d does not exist"],"addedCount":1}`)
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "task", "import", "buckets", file}, handler)
	if code != 3 || !strings.Contains(out, "Created task #21") || !strings.Contains(out, "1 item rejected:") || !strings.Contains(out, "2: destination bucket d does not exist") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	if !strings.Contains(stderr, "1 task created, 1 task rejected") || !strings.Contains(stderr, "--request-id") || strings.Contains(stderr, "may have been accepted") {
		t.Fatalf("%q", stderr)
	}
	// Unknown outcomes remain uncertain.
	handler = usabilityCreateHandler(t, `{"success":true,"partial":true,"taskIds":[21],"rejectedCount":0,"unknownCount":1,"validationErrors":[],"addedCount":1}`)
	code, _, stderr = runMockCLI(t, []string{"--lang", "en", "task", "import", "buckets", file}, handler)
	if code != 3 || !strings.Contains(stderr, "may have been accepted") || !strings.Contains(stderr, "--request-id") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestCLIUsageErrorsExitTwo(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("usage error reached server: %s", r.URL)
	}
	for _, args := range [][]string{
		{"foo"}, {"task"}, {"task", "frobnicate"}, {"task", "create"}, {"task", "show"},
		{"task", "list", "--bogus"}, {"task", "list", "--limit", "abc"}, {"task", "stop", "all"},
		{"task", "purge", "3"}, {"help", "nonexistent"}, {"node", "update", "10.0.0.2", "--address", "10.0.0.3"},
		{"task", "create", "oss-to-oss", "src/a", "dst/b", "--mapping", "add-prefix"},
		{"exceptions", "tasks", "bogus"}, {"object", "status", "cmp-1", "--bogus"},
	} {
		code, _, stderr := runMockCLI(t, append([]string{"--lang", "en"}, args...), handler)
		if code != 2 || stderr == "" {
			t.Errorf("%v: exit %d %q", args, code, stderr)
		}
	}
	// Server and operation failures stay 1.
	code, _, stderr := runMockCLI(t, []string{"task", "list"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"success":false,"message":"boom"}`)
	})
	if code != 1 || !strings.Contains(stderr, "boom") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestCLILimitErrorIsSpecific(t *testing.T) {
	code, _, stderr := runMockCLI(t, []string{"--lang", "en", "task", "list", "--limit", "abc"}, nil)
	if code != 2 || strings.Contains(stderr, ";") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestCLIHelpTopicsMatchInEveryLanguage(t *testing.T) {
	for _, topic := range helpTopics {
		if unknownHelpTopic(strings.Fields(topic)) != "" {
			t.Errorf("listed topic has no help: %s", topic)
		}
	}
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		text := helpForLocale(nil, locale)
		if strings.Contains(text, "{{") {
			t.Fatalf("%s: placeholder left in help", locale)
		}
		for _, topic := range helpTopics {
			if !strings.Contains(text, topic) {
				t.Errorf("%s help omits %s", locale, topic)
			}
		}
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "help", "nonexistent"}, nil)
	if code != 2 || out != "" || !strings.Contains(stderr, "nonexistent") || !strings.Contains(stderr, "task create") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

// stepWatch runs watch refreshes without waiting and stops after max refreshes.
func stepWatch(t *testing.T, max int) *int {
	t.Helper()
	refreshes := 0
	original := watchWait
	watchWait = func(ctx context.Context, interval time.Duration) bool {
		refreshes++
		return refreshes < max
	}
	t.Cleanup(func() { watchWait = original })
	return &refreshes
}

func TestCLIWatchSurvivesTransientFailures(t *testing.T) {
	stepWatch(t, 4)
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 || calls == 3 {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"success":false,"message":"master restarting"}`)
			return
		}
		io.WriteString(w, `{"tasks":[{"taskIndex":224,"status":"RUNNING","sourceBucket":"src","destBucket":"dst"}]}`)
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "--watch", "5", "task", "list"}, handler)
	// The fourth wait reports an interrupt, which ends the watch with 130.
	if code != 130 || calls != 4 || strings.Count(out, "Showing 1 parent task") != 2 || strings.Count(stderr, "master restarting") != 2 || !strings.Contains(stderr, "2 in a row") {
		t.Fatalf("%d calls=%d %q %q", code, calls, out, stderr)
	}
}

func TestCLIWatchStopsOnAuthFailureOrRepeatedErrors(t *testing.T) {
	refreshes := stepWatch(t, 1000)
	code, _, stderr := runMockCLI(t, []string{"--lang", "en", "--watch", "5", "task", "list"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"success":false,"message":"Sign in required"}`)
	})
	if code != 1 || *refreshes != 0 || !strings.Contains(stderr, "oss-transfer-cli login") {
		t.Fatalf("%d %d %q", code, *refreshes, stderr)
	}
	calls := 0
	code, _, stderr = runMockCLI(t, []string{"--lang", "en", "--watch", "5", "task", "list"}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"success":false,"message":"down"}`)
	})
	if code != 1 || calls != watchFailureLimit || !strings.Contains(stderr, "stopped watching after 10 consecutive") {
		t.Fatalf("%d calls=%d %q", code, calls, stderr)
	}
}

func TestCLIConnectionErrorNamesMasterAndSource(t *testing.T) {
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "unused")
	original := http.DefaultTransport
	http.DefaultTransport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	var out, errout strings.Builder
	code := runCLI([]string{"--lang", "en", "--host", "http://127.0.0.1:1", "task", "list"}, strings.NewReader(""), &out, &errout)
	if code != 1 || !strings.Contains(errout.String(), "cannot reach Master http://127.0.0.1:1 (address from the --host option)") {
		t.Fatalf("%d %q", code, errout.String())
	}
	// 403 means signed in without permission; a login hint would mislead.
	code, _, stderr := runMockCLI(t, []string{"--lang", "en", "task", "list"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"success":false,"message":"Administrator required"}`)
	})
	if code != 1 || strings.Contains(stderr, "login") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestCLIObjectStatusWaitExitReflectsOutcome(t *testing.T) {
	for _, test := range []struct {
		final string
		code  int
	}{
		{`{"status":"COMPLETED","compared":2,"matched":2}`, 0},
		{`{"status":"COMPLETED","compared":2,"matched":1,"different":1}`, exitComparisonDifferent},
		{`{"status":"COMPLETED","compared":2,"matched":1,"errors":1}`, 3},
		{`{"status":"FAILED","error":"no agent"}`, 1},
	} {
		stepWatch(t, 1000)
		calls := 0
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/master/comparisons/cmp-1" {
				t.Fatalf("unexpected %s", r.URL)
			}
			calls++
			if calls < 3 {
				io.WriteString(w, `{"status":"RUNNING","compared":1}`)
				return
			}
			io.WriteString(w, test.final)
		}
		code, out, stderr := runMockCLI(t, []string{"--json", "object", "status", "cmp-1", "--wait"}, handler)
		var final map[string]interface{}
		if code != test.code || calls != 3 || json.Unmarshal([]byte(out), &final) != nil || final["status"] == "RUNNING" {
			t.Fatalf("%s: %d calls=%d %q %q", test.final, code, calls, out, stderr)
		}
	}
	// --watch alone keeps polling a finished comparison and prints every frame.
	stepWatch(t, 2)
	code, out, _ := runMockCLI(t, []string{"--watch", "2", "--json", "object", "status", "cmp-1"}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"COMPLETED","compared":2,"matched":2}`)
	})
	if code != 130 || strings.Count(out, "\n") != 2 {
		t.Fatalf("%d %q", code, out)
	}
}
