package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runMockCLI(t *testing.T, args []string, handler http.HandlerFunc) (int, string, string) {
	t.Helper()
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	original := http.DefaultTransport
	http.DefaultTransport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		handler(rec, r)
		return rec.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	var out, errout bytes.Buffer
	status := runCLI(append([]string{"--host", "http://cli-test.invalid"}, args...), strings.NewReader(""), &out, &errout)
	return status, out.String(), errout.String()
}
func TestCLIPartialJSONAndHumanExit(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, map[string]interface{}{"tasks": []interface{}{map[string]interface{}{"taskIndex": 224, "status": "RUNNING", "sourceBucket": "src", "destBucket": "dst", "putObjectCount": 12, "listObjectCount": 20}}, "partial": true, "unavailableWorkers": []string{"10.0.0.20"}})
	}
	for _, jsonMode := range []bool{false, true} {
		args := []string{"task", "list"}
		if jsonMode {
			args = append(args, "--json")
		}
		code, out, stderr := runMockCLI(t, args, handler)
		if code != 3 || !strings.Contains(stderr, "10.0.0.20") {
			t.Fatalf("%d %s %s", code, out, stderr)
		}
		if jsonMode {
			var data map[string]interface{}
			if json.Unmarshal([]byte(out), &data) != nil {
				t.Fatal(out)
			}
		} else if !strings.Contains(out, "#224") {
			t.Fatal(out)
		}
	}
}
func TestCLIFailureExitAndCleanJSON(t *testing.T) {
	code, out, stderr := runMockCLI(t, []string{"node", "list", "--json"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, `{"success":false,"message":"offline"}`)
	})
	var data map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &data) != nil || !strings.Contains(stderr, "offline") || data["success"] != false {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}
func TestGlobalOptionsAndReadOnlyWatch(t *testing.T) {
	for _, args := range [][]string{{"--watch", "0", "task", "list"}, {"--watch", "1.5s", "task", "list"}, {"--host", "http://u:p@host", "task", "list"}, {"--watch", "1", "task", "stop", "224"}, {"--json", "count-lines", "x"}} {
		code, _, _ := runMockCLI(t, args, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected network") })
		if code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
	options, err := parseGlobals([]string{"task", "show", "224", "--json", "--watch=1"})
	if err != nil || options.watch.Seconds() != 1 || !options.json {
		t.Fatalf("%+v %v", options, err)
	}
	options, err = parseGlobals([]string{"task", "create", "--", "--json"})
	if err != nil || options.json {
		t.Fatalf("end of options ignored: %+v %v", options, err)
	}
}
func TestCLIHelpAndAliases(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help", "task", "create"}, {"storage", "--help"}, {"--version"}} {
		code, out, _ := runMockCLI(t, args, func(http.ResponseWriter, *http.Request) { t.Fatal("network during help") })
		if code != 0 || out == "" {
			t.Fatalf("%d %q", code, out)
		}
	}
	args, err := normalizeCLICommand([]string{"import-tasks", "old.csv"})
	if err != nil || !reflect.DeepEqual(args, []string{"task", "import-legacy", "old.csv"}) {
		t.Fatalf("%v %v", args, err)
	}
	args, err = normalizeCLICommand([]string{"add-task", "a/b", "c/d", "--dateAfter=2026-09-01"})
	if err != nil || args[len(args)-1] != "--date-after=2026-09-01" {
		t.Fatalf("%v %v", args, err)
	}
	args, err = normalizeCLICommand([]string{"set-rate-limit", "bandwidth", "100M", "10.0.0.2"})
	if err != nil || !reflect.DeepEqual(args, []string{"limit", "set", "bandwidth", "100", "--unit", "Mbps", "--worker", "10.0.0.2"}) {
		t.Fatalf("%v %v", args, err)
	}
}
func TestAuthSessionLifecycleAndIsolation(t *testing.T) {
	dir := t.TempDir()
	original := sessionConfigDir
	sessionConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { sessionConfigDir = original })
	const host = "http://cli-test.invalid"
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" {
			t.Fatal(r.URL)
		}
		body := adminTestBody(t, r)
		if body["password"] != "  secret  " || body["username"] != "admin" {
			t.Fatal("incorrect credential handling")
		}
		io.WriteString(w, `{"success":true,"token":"session-secret","user":{"username":"admin"}}`)
	})
	result, err := runAuthCommand(client, "login", []string{"--username", "admin", "--password-stdin"}, strings.NewReader("  secret  \n"), host)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["token"]; ok {
		t.Fatal("token leaked")
	}
	token, err := loadSession(host)
	if err != nil || token != "session-secret" {
		t.Fatal("session load", err)
	}
	if other, err := loadSession("http://other.invalid"); err != nil || other != "" {
		t.Fatal("host isolation")
	}
	path, _ := sessionPath(host)
	testSessionPermissions(t, path, host)
	client = adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/logout" {
			t.Fatal(r.URL)
		}
		w.WriteHeader(503)
		io.WriteString(w, `{"message":"offline"}`)
	})
	if _, err := runAuthCommand(client, "logout", nil, nil, host); err == nil {
		t.Fatal("logout failure hidden")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("local session not removed", err)
	}
}
func TestAuthSecretsAndRedaction(t *testing.T) {
	t.Setenv("CLI_TEST_PASSWORD", "secret with spaces ")
	opts, _ := parseCommandOptions([]string{"--password-env", "CLI_TEST_PASSWORD"}, []string{"password-env"}, nil)
	got, err := authSecret(opts, "password", nil, true)
	if err != nil || got != "secret with spaces " {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "password")
	os.WriteFile(path, []byte(" file secret \r\n"), 0600)
	opts, _ = parseCommandOptions([]string{"--password-file", path}, []string{"password-file"}, nil)
	got, err = authSecret(opts, "password", nil, true)
	if err != nil || got != " file secret " {
		t.Fatal(err)
	}
	data := sanitizeOutput(map[string]interface{}{"token": "a", "user": map[string]interface{}{"passwordHash": "b"}, "data": []interface{}{map[string]interface{}{"secretKey": "c", "accessKey": "d"}}})
	encoded, _ := json.Marshal(data)
	if strings.Contains(string(encoded), `:"a"`) || strings.Contains(string(encoded), `:"b"`) || strings.Contains(string(encoded), `:"c"`) || strings.Contains(string(encoded), `:"d"`) {
		t.Fatal("secrets not redacted")
	}
	for _, args := range [][]string{{"--username", "admin"}, {"--username", "admin", "--password-env", "CLI_TEST_PASSWORD", "--password-stdin"}} {
		client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid login reached server") })
		if _, err := runAuthCommand(client, "login", args, strings.NewReader("secret"), "http://invalid"); err == nil {
			t.Fatal(fmt.Sprint(args))
		}
	}
}

func TestSessionTokensAreRedactedRecursively(t *testing.T) {
	encoded, err := json.Marshal(sanitizeOutput(map[string]interface{}{"configs": []interface{}{map[string]interface{}{"sessionToken": "temporary-secret", "session_token": "other-secret"}}, "requestId": "safe-id"}))
	if err != nil || strings.Contains(string(encoded), "temporary-secret") || strings.Contains(string(encoded), "other-secret") || !strings.Contains(string(encoded), "safe-id") {
		t.Fatalf("redaction failed: %s %v", encoded, err)
	}
}
