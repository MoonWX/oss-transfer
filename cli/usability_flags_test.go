package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCreateErrorsNameTheFlagsTyped(t *testing.T) {
	client := creationTestClient(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--mapping", "add-prefix"}, "--mapping add-prefix needs a nonempty --target-prefix"},
		{[]string{"--mapping", "replace-prefix"}, "--mapping replace-prefix needs a nonempty --prefix"},
		{[]string{"--mapping", "exact-key", "--target-key", "x"}, "--mapping exact-key needs --object-key"},
		{[]string{"--mapping", "bogus"}, "--mapping must be preserve, add-prefix, replace-prefix or exact-key"},
		{[]string{"--mapping", "add-prefix", "--target-prefix", "p/", "--target-key", "k"}, "--mapping add-prefix does not use --target-key"},
		{[]string{"--target-prefix", "x/"}, "--mapping add-prefix"},
		{[]string{"--target-key", "x"}, "--mapping exact-key"},
		{[]string{"--date-after", "2026-13-01"}, "Invalid --date-after"},
		{[]string{"--date-after", "2026-09-02", "--date-before", "2026-09-01"}, "--date-before must be later than --date-after"},
	} {
		_, err := runTaskCreate(client, append([]string{"oss-to-oss", "src/a", "dst/b"}, test.args...))
		got := localizedCLIError(localeEN, err)
		if err == nil || !isUsageError(err) || !strings.Contains(got, test.want) {
			t.Errorf("%v: %q", test.args, got)
		}
		for _, internal := range []string{"ADD_PREFIX", "targetPrefix", "dateAfter", "keyMapping"} {
			if strings.Contains(got, internal) {
				t.Errorf("%v: internal name %s in %q", test.args, internal, got)
			}
		}
	}
	_, err := runTaskCreate(client, []string{"oss-to-local", "src/a", "--binding", "10.0.0.1=/data", "--target-relative-path", "x"})
	if got := localizedCLIError(localeEN, err); !strings.Contains(got, "--target-relative-path only applies") {
		t.Fatal(got)
	}
}

func TestCreateAcceptsDateOnlyLikeTheServer(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange(), creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"taskIds":[3]}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["dateAfter"] != "2026-09-01T00:00:00" || rows[0]["dateBefore"] != "2026-09-02T12:30:00" {
				t.Fatal(rows)
			}
		},
	})
	if _, err := runTaskCreate(client, []string{"oss-to-oss", "src/a", "dst/b", "--date-after", "2026-09-01", "--date-before", "2026-09-02 12:30"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWarnsThatUploadDistributionIsIgnored(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/local-transfers/validate":
			io.WriteString(w, `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"}]}}`)
		case "/api/master/local-transfers":
			io.WriteString(w, `{"success":true,"data":{"acceptedCount":1,"taskIds":[8],"parentTaskIds":[8],"rejectedCount":0}}`)
		default:
			t.Fatalf("unexpected %s", r.URL)
		}
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "task", "create", "local-to-oss", "dst/b", "--binding", "10.0.0.1=/data", "--distribution", "balanced"}, handler)
	if code != 0 || !strings.Contains(stderr, "--distribution/--balanced is ignored") || !strings.Contains(out, "Created task #8 (local-to-oss 10.0.0.1=/data → dst/b)") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	code, _, stderr = runMockCLI(t, []string{"--lang", "en", "task", "create", "local-to-oss", "dst/b", "--binding", "10.0.0.1=/data"}, handler)
	if code != 0 || strings.Contains(stderr, "ignored") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestNodeAndWorkerFlagsAreInterchangeable(t *testing.T) {
	for _, test := range []struct {
		args  []string
		query string
	}{
		{[]string{"task", "list", "--node", "10.0.0.2"}, "workerNode=10.0.0.2"},
		{[]string{"task", "list", "--worker", "10.0.0.2"}, "workerNode=10.0.0.2"},
	} {
		code, _, stderr := runMockCLI(t, test.args, func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.RawQuery, test.query) {
				t.Fatalf("%v: %s", test.args, r.URL.RawQuery)
			}
			io.WriteString(w, `{"tasks":[]}`)
		})
		if code != 0 {
			t.Fatalf("%v: %d %s", test.args, code, stderr)
		}
	}
	// A command that takes --node also accepts --worker.
	opts, err := parseCommandOptions([]string{"--worker", "10.0.0.2"}, []string{"node"}, nil)
	if err != nil || opts.Value("node") != "10.0.0.2" {
		t.Fatal(opts, err)
	}
	opts, err = parseCommandOptions([]string{"--node=10.0.0.3"}, []string{"worker"}, nil)
	if err != nil || opts.Value("worker") != "10.0.0.3" {
		t.Fatal(opts, err)
	}
	// Commands with neither flag still reject both.
	if _, err := parseCommandOptions([]string{"--node", "x"}, []string{"label"}, nil); err == nil {
		t.Fatal("unknown --node accepted")
	}
}

func TestTaskListShowsTotalsAndFilterScope(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"tasks":[
			{"taskIndex":3,"status":"RUNNING","putObjectCount":2,"transferredBytes":2048,"failedObjectCount":0},
			{"taskIndex":2,"status":"FAILED","putObjectCount":1,"putObjectSize":1024,"failedObjectCount":4}
		],"hasMore":true,"nextBeforeId":2}`)
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "task", "list"}, handler)
	if code != 0 || !strings.Contains(out, "Total: 2 tasks, 1 running, 0 completed, 1 failed; 3 KiB transferred (3 objects), 4 failed objects") || strings.Contains(out, "Filters covered") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	code, out, _ = runMockCLI(t, []string{"--lang", "en", "task", "list", "--status", "failed"}, handler)
	if code != 0 || !strings.Contains(out, "Filters covered only the newest 100 tasks; add --all") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestExceptionTasksUsesConsoleEndpoint(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/exceptions/tasks" || r.URL.Query().Get("type") != "failed" || r.URL.Query().Get("search") != "224" || r.URL.Query().Get("limit") != "20" {
			t.Fatalf("unexpected %s", r.URL)
		}
		io.WriteString(w, `{"success":true,"data":{"items":[{"taskIndex":224,"status":"PARTIAL_FAILED","sourceBucket":"src","destBucket":"dst","failedObjectCount":3}],"hasMore":false}}`)
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "exceptions", "tasks", "failed", "--search", "#224", "--limit", "20"}, handler)
	if code != 0 || !strings.Contains(out, "224") || !strings.Contains(out, "Tasks with exception records: 1") || !strings.Contains(out, "exceptions list failed --task-id 224") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	code, out, _ = runMockCLI(t, []string{"--json", "exceptions", "tasks", "failed", "--search", "224", "--limit", "20"}, handler)
	var result map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &result) != nil || len(result["exceptionTasks"].([]interface{})) != 1 {
		t.Fatalf("%d %s", code, out)
	}
}

func TestWatchStopsOnPermanentHTTPErrors(t *testing.T) {
	for status, want := range map[int]bool{400: false, 401: false, 403: false, 404: false, 409: false, 429: true, 500: true, 503: true} {
		if got := watchRetryable(&apiHTTPError{Status: status}); got != want {
			t.Errorf("watchRetryable(HTTP %d) = %v, want %v", status, got, want)
		}
	}
}

func TestNodeAliasErrorsNameTypedFlag(t *testing.T) {
	_, err := parseCommandOptions([]string{"--node"}, []string{"worker"}, nil)
	if err == nil || !strings.Contains(err.Error(), "--node") || strings.Contains(err.Error(), "--worker") {
		t.Fatalf("error = %v, want it to name --node", err)
	}
	_, err = adminOptions([]string{"--node", "a", "--node", "b"}, []string{"worker"}, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "--node/--worker") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestDashLeadingIDsAreArguments(t *testing.T) {
	id := "-bwMV38HaZT0QmE6QCGzVCXUgga0wZKK1nxzDyBPK6o"
	o, err := parseCommandOptions([]string{id, "--wait"}, nil, []string{"wait"})
	if err != nil || len(o.Positionals) != 1 || o.Positionals[0] != id || !o.Switches["wait"] {
		t.Fatalf("options = %+v, err = %v", o, err)
	}
	if _, err := parseCommandOptions([]string{"-josn"}, nil, []string{"json"}); err == nil {
		t.Fatal("a mistyped short option must still be rejected")
	}
}
