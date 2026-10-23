package master

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

func TestStorageCredentialsAreWriteOnlyAndPreservedOnEdit(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent, _ := taskAPIAgent(t, f)
	secretValues := []string{"private-fixture-access", "private-fixture-secret", "private-fixture-session", "rotated-fixture-access", "rotated-fixture-secret"}
	assertPublic := func(body map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		for _, value := range secretValues {
			if strings.Contains(string(encoded), value) {
				t.Fatal("management response disclosed a credential")
			}
		}
	}
	create := map[string]any{"alias": "private", "endpoint": "https://s3.example.test", "accessKey": secretValues[0], "secretKey": secretValues[1], "sessionToken": secretValues[2], "region": "test-region", "validate": false}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, create)
	if code != 200 || body["partial"] != false {
		t.Fatalf("create or Agent synchronization failed: %d", code)
	}
	assertPublic(body)
	code, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/s3-config", f.admin, nil)
	if code != 200 {
		t.Fatal(code)
	}
	assertPublic(body)
	public := body["configs"].([]any)[0].(map[string]any)
	for _, field := range []string{"accessKey", "secretKey", "sessionToken"} {
		if _, present := public[field]; present {
			t.Fatalf("public connection contains %s", field)
		}
	}
	if public["credentialsConfigured"] != true || public["sessionTokenConfigured"] != true {
		t.Fatal("credential configuration status is missing")
	}
	edit := map[string]any{"alias": "private", "endpoint": "https://changed.example.test", "region": "test-region", "accessKey": "", "secretKey": "", "validate": false}
	code, body = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/s3-config/private", f.admin, edit)
	if code != 200 {
		t.Fatal(code)
	}
	assertPublic(body)
	assertStored := func(access, secret, token string) {
		t.Helper()
		master, err := storageconfig.NewMaster(f.db).Snapshot()
		if err != nil || len(master.Connections) != 1 {
			t.Fatal("cannot read persisted configuration")
		}
		worker, err := agent.Configs.Snapshot()
		if err != nil || len(worker.Connections) != 1 {
			t.Fatal("cannot read Agent configuration")
		}
		for _, snapshot := range []storageconfig.Snapshot{master, worker} {
			config := snapshot.Connections[0]
			if config.AccessKey != access || config.SecretKey != secret || config.SessionToken != token || config.Endpoint != edit["endpoint"] {
				t.Fatal("stored credentials or Agent synchronization differ from the requested change")
			}
		}
	}
	assertStored(secretValues[0], secretValues[1], secretValues[2])
	edit["accessKey"] = secretValues[3]
	code, body = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/s3-config/private", f.admin, edit)
	if code != 400 {
		t.Fatal("partial credential replacement was accepted")
	}
	assertPublic(body)
	assertStored(secretValues[0], secretValues[1], secretValues[2])
	edit["secretKey"] = secretValues[4]
	code, body = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/s3-config/private", f.admin, edit)
	if code != 200 {
		t.Fatal("credential rotation failed", code)
	}
	assertPublic(body)
	assertStored(secretValues[3], secretValues[4], "")
	if code, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/s3-config", "", nil); code != 401 {
		t.Fatal("unauthenticated connection access accepted")
	}
}

func TestStorageProviderErrorDoesNotReturnCredentials(t *testing.T) {
	config := storageconfig.Config{AccessKey: "fixture-access", SecretKey: "fixture-access-with-secret", SessionToken: "fixture-token"}
	err := redactStorageError(&AgentError{Status: 403, Message: "access=fixture-access secret=fixture-access-with-secret token=fixture-token"}, []storageconfig.Config{config})
	if err.Error() != "access=[redacted] secret=[redacted] token=[redacted]" || !agentStatus(err, 403) {
		t.Fatal("provider error leaked credential fragments or lost HTTP status")
	}
}
