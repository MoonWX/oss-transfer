package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type adminTestTransport func(*http.Request) (*http.Response, error)

func (transport adminTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// Execute the real HTTP request/response path through a mock handler without
// opening a socket or reaching any configured transfer service.
func adminTestClient(t *testing.T, handler http.HandlerFunc) *ApiClient {
	t.Helper()
	return &ApiClient{
		baseUrl: "http://admin-cli-test.invalid/api/master",
		client: &http.Client{Transport: adminTestTransport(func(request *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			response.Header().Set("Content-Type", "application/json")
			handler(response, request)
			return response.Result(), nil
		})},
	}
}

func adminTestJSON(t *testing.T, response http.ResponseWriter, value interface{}) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func adminTestBody(t *testing.T, request *http.Request) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAdminRateConversionAndExplicitScope(t *testing.T) {
	cases := []struct {
		value, unit string
		want        float64
	}{
		{"200", "", 200e6}, {"0.5", "Gbps", 500e6}, {"800", "Kbps", 800e3}, {"10", "bps", 10}, {"unlimited", "", 0},
	}
	for _, test := range cases {
		t.Run(test.value+test.unit, func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/master/rate-limit/bandwidth/10.0.0.2" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if got := adminTestBody(t, r)["qps"]; got != test.want {
					t.Fatalf("rate = %v, want %v", got, test.want)
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true})
			})
			args := []string{"set", "bandwidth", test.value, "--worker", "10.0.0.2"}
			if test.unit != "" {
				args = append(args, "--unit", test.unit)
			}
			if _, err := runAdminCommand(client, "limit", args); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
	invalid := [][]string{
		{"set", "bandwidth", "NaN", "--all"}, {"set", "bandwidth", "Inf", "--all"},
		{"set", "bandwidth", "typo", "--all"}, {"set", "bandwidth", "0", "--all"},
		{"set", "bandwidth", "1e309", "--all"}, {"set", "bandwidth", "100"},
		{"set", "put", "100", "--all", "--worker", "10.0.0.2"},
		{"set", "put", "100", "--all", "--unit", "Mbps"},
		{"set", "bandwidth", "100", "--all", "--unit", "MBps"},
		{"history", "--limit", "-1"}, {"history", "--limit", "1001"},
	}
	for _, args := range invalid {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("invalid arguments performed a request: %v", args)
		})
		if _, err := runAdminCommand(client, "limit", args); err == nil {
			t.Errorf("accepted invalid args: %v", args)
		}
	}
}

func TestAdminDestructiveCommandsRequireTargetsAndConfirmation(t *testing.T) {
	cases := []struct {
		group string
		args  []string
	}{
		{"node", []string{"remove", "10.0.0.2"}}, {"node", []string{"remove", "--yes"}},
		{"storage", []string{"remove", "source"}}, {"storage", []string{"remove", "--yes"}},
		{"exceptions", []string{"clear", "failed", "--yes"}}, {"exceptions", []string{"clear", "failed", "--all"}},
		{"exceptions", []string{"clear", "local", "--task-id", "0", "--yes"}},
		{"exceptions", []string{"clear", "fallback", "--all", "--yes", "--task-id", "224"}},
	}
	for _, test := range cases {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatalf("unsafe call: %s %v", test.group, test.args) })
		if _, err := runAdminCommand(client, test.group, test.args); err == nil {
			t.Errorf("accepted unsafe command: %s %v", test.group, test.args)
		}
	}
}

func TestAdminNodeUpdatePreservesLabel(t *testing.T) {
	calls := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.Method != "GET" || r.URL.Path != "/api/master/nodes" {
				t.Fatalf("unexpected registry request: %s %s", r.Method, r.URL)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2", "label": "迁移节点"}}}})
			return
		}
		if r.Method != "PUT" || r.URL.Path != "/api/master/nodes/10.0.0.2" {
			t.Fatalf("unexpected update: %s %s", r.Method, r.URL)
		}
		if body := adminTestBody(t, r); body["label"] != "迁移节点" || body["address"] != "10.0.0.3" {
			t.Fatalf("unexpected body: %#v", body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	if _, err := runAdminCommand(client, "node", []string{"update", "10.0.0.2", "--address", "10.0.0.3"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	for _, address := range []string{"10.0.0.2:2222", "host/name", "01.2.3.4", "224.0.0.1"} {
		if _, err := runAdminCommand(client, "node", []string{"add", address}); err == nil {
			t.Errorf("invalid IPv4 accepted: %s", address)
		}
	}
}

func TestAdminNodeListPreservesOfflineNode(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/nodes":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2"}, map[string]interface{}{"address": "10.0.0.3"}}}})
		case "/api/master/worker-status":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "workers": map[string]interface{}{"10.0.0.2": map[string]interface{}{"success": true}}})
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
	})
	result, err := runAdminCommand(client, "node", []string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	if result["complete"] != false || !reflect.DeepEqual(result["failedWorkers"], []string{"10.0.0.3"}) {
		t.Fatalf("offline node lost: %#v", result)
	}
}

func TestAdminStorageCredentialsAndOptions(t *testing.T) {
	t.Setenv("CLI_TEST_ACCESS", "test-access")
	t.Setenv("CLI_TEST_SESSION", "test-session")
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/master/s3-config" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		body := adminTestBody(t, r)
		want := map[string]interface{}{"alias": "source", "endpoint": "https://oss.example", "region": "cn-north-1", "accessKey": "test-access", "secretKey": "test-secret", "sessionToken": "test-session", "validate": true, "useV2SignerType": false, "pathStyleEnable": nil}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("body = %#v", body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "partial": true, "failedCount": 1})
	})
	result, err := runAdminCommand(client, "storage", []string{"add", "source", "--endpoint", "https://oss.example", "--region", "cn-north-1", "--access-key-env", "CLI_TEST_ACCESS", "--secret-key-file", secretFile, "--session-token-env", "CLI_TEST_SESSION", "--v2-signer", "false", "--path-style", "inherit"})
	if err != nil {
		t.Fatal(err)
	}
	if result["partial"] != true {
		t.Fatal("partial synchronization was discarded")
	}
	if encoded, _ := json.Marshal(result); bytes.Contains(encoded, []byte("test-secret")) {
		t.Fatal("secret exposed in result")
	}
}

func TestAdminStorageUpdateMergesAndListRedacts(t *testing.T) {
	calls := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == "GET" {
			adminTestJSON(t, w, map[string]interface{}{"success": true, "configs": []interface{}{map[string]interface{}{"alias": "source", "endpoint": "https://old.example", "region": "cn-north-1", "accessKey": "keep-access", "secretKey": "keep-secret", "sessionToken": "keep-session", "pathStyleEnable": true, "useV2SignerType": false}}})
			return
		}
		if r.Method != "PUT" || r.URL.Path != "/api/master/s3-config/source" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		body := adminTestBody(t, r)
		if body["endpoint"] != "https://new.example" || body["region"] != "cn-north-1" || body["sessionToken"] != nil || body["secretKey"] != nil || body["accessKey"] != nil || body["pathStyleEnable"] != true || body["useV2SignerType"] != false {
			t.Fatalf("update discarded fields: %#v", body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	if _, err := runAdminCommand(client, "storage", []string{"update", "source", "--endpoint", "https://new.example"}); err != nil {
		t.Fatal(err)
	}
	result, err := runAdminCommand(client, "storage", []string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("keep-secret")) || bytes.Contains(encoded, []byte("keep-access")) || bytes.Contains(encoded, []byte("keep-session")) {
		t.Fatalf("credential leaked: %s", encoded)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestAdminStorageRegionAndTemporaryCredentials(t *testing.T) {
	t.Setenv("CLI_TEST_SESSION", "rotated-session")
	file := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(file, []byte("file-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		flags         []string
		region, token string
	}{
		{"rotate from environment", []string{"--region", "cn-east-2", "--session-token-env", "CLI_TEST_SESSION"}, "cn-east-2", "rotated-session"},
		{"rotate from file", []string{"--session-token-file", file}, "cn-north-1", "file-session"},
		{"explicitly clear", []string{"--clear-session-token"}, "cn-north-1", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					adminTestJSON(t, w, map[string]interface{}{"success": true, "configs": []interface{}{map[string]interface{}{
						"alias": "source", "endpoint": "https://oss.example", "region": "cn-north-1", "accessKey": "keep-access", "secretKey": "keep-secret", "sessionToken": "old-session"}}})
					return
				}
				writes++
				body := adminTestBody(t, r)
				if r.Method != "PUT" || body["region"] != test.region || body["sessionToken"] != test.token || body["accessKey"] != nil || body["secretKey"] != nil {
					t.Fatalf("incorrect storage update: %#v", body)
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true})
			})
			if _, err := runStorageAdmin(client, "update", append([]string{"source"}, test.flags...)); err != nil {
				t.Fatal(err)
			}
			if writes != 1 {
				t.Fatalf("writes = %d", writes)
			}
		})
	}
}

func TestAdminStorageInvalidCredentialSourceDoesNotWrite(t *testing.T) {
	t.Setenv("CLI_TEST_EMPTY", "")
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("invalid storage command made API call") })
	for _, extra := range [][]string{
		{"--secret-key-env", "CLI_TEST_EMPTY"},
		{"--secret-key-env", "CLI_TEST_UNSET_VALUE_912"},
		{"--secret-key-file", "/nonexistent-cli-test-secret"},
		{"--secret-key-env", "CLI_TEST_EMPTY", "--secret-key-file", "/any"},
		{"--secret-key", "plain-secret"},
		{"--session-token-env", "CLI_TEST_EMPTY"},
		{"--session-token-env", "CLI_TEST_EMPTY", "--session-token-file", "/any"},
		{"--session-token", "plain-session"},
		{"--clear-session-token", "--session-token-env", "CLI_TEST_EMPTY"},
	} {
		args := append([]string{"add", "source", "--endpoint", "https://oss.example"}, extra...)
		if _, err := runAdminCommand(client, "storage", args); err == nil {
			t.Errorf("accepted invalid secret flags: %v", extra)
		}
	}
}

func TestAdminExceptionFallbackChecksMembershipAndTruncation(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/fallback":
			body := adminTestBody(t, r)
			if body["taskIndex"] != float64(224) || body["limit"] != float64(10) {
				t.Fatalf("wrong filters: %#v", body)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "fallbackObjects": map[string]interface{}{"10.0.0.2": []interface{}{map[string]interface{}{"key": "example"}}, "10.0.0.2Count": 40}})
		case "/api/master/nodes":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2"}, map[string]interface{}{"address": "10.0.0.3"}}}})
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
	})
	result, err := runAdminCommand(client, "exceptions", []string{"list", "fallback", "--task-id", "224", "--limit", "10"})
	if err != nil {
		t.Fatal(err)
	}
	if result["complete"] != false || result["partial"] != true || result["truncated"] != true || result["total"] != float64(40) {
		t.Fatalf("lost completeness: %#v", result)
	}
	if !reflect.DeepEqual(result["failedWorkers"], []string{"10.0.0.3"}) {
		t.Fatalf("missing worker lost: %#v", result)
	}
	row := adminArray(result["items"])[0].(map[string]interface{})
	if row["workerNode"] != "10.0.0.2" {
		t.Fatalf("missing row origin: %#v", row)
	}
}

func TestAdminExceptionCSVExportProtectsFormulaAndExistingFiles(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/nodes" {
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"address": "10.0.0.2"}}}})
			return
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "complete": true, "failedKeys": map[string]interface{}{"10.0.0.2": []interface{}{map[string]interface{}{"key": "=malicious()", "errorMessage": "message,\"quoted\"\nnext", "retryStatus": "FAILED", "retryCount": 2, "retryUpdatedAt": int64(1793011200000), "retryError": "retry,\"failed\"\nagain", "retryGeneration": 3, "retryExecutionNodeIp": "agent-next", "retryExecutionNodeDisplay": map[string]interface{}{"displayName": "Next", "displayAddress": "10.0.0.3"}}}, "10.0.0.2Count": 1}})
	})
	path := filepath.Join(t.TempDir(), "failures.csv")
	args := []string{"export", "failed", "--output", path}
	result, err := runAdminCommand(client, "exceptions", args)
	if err != nil {
		t.Fatal(err)
	}
	if result["exported"] != 1 || result["complete"] != true {
		t.Fatalf("unexpected export result: %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(content), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][6] != "'=malicious()" || rows[1][11] != "message,\"quoted\"\nnext" {
		t.Fatalf("CSV did not preserve safe content: %#v", rows[1])
	}
	columns := map[string]string{}
	for i, name := range rows[0] {
		columns[name] = rows[1][i]
	}
	for key, expected := range map[string]string{"retryStatus": "FAILED", "retryCount": "2", "retryUpdatedAt": "1793011200000", "retryError": "retry,\"failed\"\nagain", "retryGeneration": "3", "retryExecutionNode": "Next (10.0.0.3)", "retryExecutionNodeIp": "agent-next"} {
		if columns[key] != expected {
			t.Fatalf("CSV lost retry result %s: %#v", key, columns)
		}
	}
	if _, err := runAdminCommand(client, "exceptions", args); err == nil {
		t.Fatal("existing export silently overwritten")
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, content) {
		t.Fatal("existing export was modified")
	}
}

func TestAdminLocalExceptionPaginationAndServerCapAreVisible(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/local-transfers/failures" || r.URL.Query().Get("direction") != "OSS_TO_LOCAL" || r.URL.Query().Get("taskId") != "224" || r.URL.Query().Get("offset") != "25" || r.URL.Query().Get("limit") != "25" {
			t.Fatalf("wrong local failure query: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": []interface{}{}, "total": 1200, "partial": false, "workers": map[string]interface{}{"10.0.0.2": map[string]interface{}{"total": 1200, "items": []interface{}{map[string]interface{}{"key": "one"}}}}}})
	})
	result, err := runAdminCommand(client, "exceptions", []string{"list", "local", "--task-id", "224", "--direction", "OSS_TO_LOCAL", "--offset", "25"})
	if err != nil {
		t.Fatal(err)
	}
	if result["truncated"] != true || result["hasMore"] != true || !reflect.DeepEqual(result["truncatedWorkers"], []string{"10.0.0.2"}) {
		t.Fatalf("hidden backend truncation: %#v", result)
	}
}

func TestAdminLocalExportRejectsIgnoredFiltersAndStreamsReport(t *testing.T) {
	var zipContent bytes.Buffer
	archive := zip.NewWriter(&zipContent)
	entry, err := archive.Create("summary.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("taskId,status\n224,COMPLETED\n")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/api/master/local-transfers/report" {
			t.Fatalf("wrong report request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipContent.Bytes())
	})
	path := filepath.Join(t.TempDir(), "report.zip")
	if _, err := runAdminCommand(client, "exceptions", []string{"export", "local", "--task-id", "224", "--output", path}); err == nil {
		t.Fatal("ignored local report filter accepted")
	}
	if calls != 0 {
		t.Fatal("filtered export requested whole-cluster report")
	}
	if _, err := runAdminCommand(client, "exceptions", []string{"export", "local", "--output", path}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !bytes.Equal(content, zipContent.Bytes()) {
		t.Fatalf("bad report bytes: %q", content)
	}
}

func TestAdminObjectComparePreservesExactKeysAndPartialResponse(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sourceKey") != " a+b/&?#对象 " || r.URL.Query().Get("destKey") != "dest/file" || r.URL.Query().Get("sourceBucket") != "source/my-bucket" {
			t.Fatalf("object identifiers were changed: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "partial": true, "complete": false, "sourceMetadata": map[string]interface{}{"exists": true}, "destMetadata": map[string]interface{}{"queryStatus": "UNAVAILABLE"}})
	})
	args := []string{"compare", "--source-bucket", "source/my-bucket", "--source-key", " a+b/&?#对象 ", "--dest-bucket", "dest/other-bucket", "--dest-key", "dest/file"}
	result, err := runAdminCommand(client, "object", args)
	if err != nil {
		t.Fatal(err)
	}
	if result["partial"] != true || result["complete"] != false {
		t.Fatalf("partial comparison lost: %#v", result)
	}
}

func TestAdminHistoryAndAdjustmentKeepPagination(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/rate-limit/history" {
			if r.URL.Query().Get("limit") != "50" {
				t.Fatalf("default limit changed: %s", r.URL)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "partial": true, "failedWorkers": []string{"10.0.0.3"}})
			return
		}
		if r.URL.Query().Get("workerIp") != "10.0.0.2" || r.URL.Query().Get("eventId") != "event/&+" || r.URL.Query().Get("cursor") != "next+page" {
			t.Fatalf("adjustment query changed: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "historyComplete": false, "nextCursor": "later", "done": false})
	})
	if result, err := runAdminCommand(client, "limit", []string{"history"}); err != nil || result["partial"] != true {
		t.Fatalf("history: %#v, %v", result, err)
	}
	result, err := runAdminCommand(client, "limit", []string{"adjustment", "--worker", "10.0.0.2", "--event-id", "event/&+", "--cursor", "next+page"})
	if err != nil || result["historyComplete"] != false || result["nextCursor"] != "later" {
		t.Fatalf("adjustment: %#v, %v", result, err)
	}
}

func TestAdminFallbackClearDoesNotClaimConfirmedCompletion(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/master/fallback/clear" {
			t.Fatalf("unexpected clear: %s %s", r.Method, r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "message": "records cleared"})
	})
	result, err := runAdminCommand(client, "exceptions", []string{"clear", "fallback", "--all", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if result["complete"] != false || result["warning"] == nil {
		t.Fatalf("unconfirmed clear reported complete: %#v", result)
	}
}

func TestExceptionClearPreservesFilterAndRequiresSafeMaster(t *testing.T) {
	for _, kind := range []string{"failed", "local"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					if r.URL.Path != "/api/master/failed" {
						t.Fatalf("unexpected probe %s", r.URL)
					}
					adminTestJSON(t, w, map[string]interface{}{"success": true, "clearableCount": 2})
					return
				}
				if r.URL.Path != "/api/master/failed/clear" {
					t.Fatalf("unexpected clear %s", r.URL)
				}
				body := adminTestBody(t, r)
				if body["type"] != kind || body["taskIndex"] != float64(43) || body["workerNode"] != "agent-1" {
					t.Fatalf("lost filters: %#v", body)
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "clearedCount": 2, "scope": "resolved-exceptions"})
			})
			result, err := runAdminCommand(client, "exceptions", []string{"clear", kind, "--task-id", "43", "--worker", "agent-1", "--yes"})
			if err != nil || result["clearedCount"] != float64(2) || calls != 2 {
				t.Fatalf("clear: %#v %v calls=%d", result, err, calls)
			}
		})
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/failed" {
			t.Fatalf("unsafe clear on older master: %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	if _, err := runAdminCommand(client, "exceptions", []string{"clear", "failed", "--all", "--yes"}); err == nil {
		t.Fatal("accepted old unsafe API")
	}
}
