package master

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func openLocalCLIFixture(t *testing.T, f *agentHTTPFixture) string {
	t.Helper()
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var password string
	f.master, password, err = New(f.db, Options{DataDirectory: filepath.Dir(f.path), LocalCLIEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	f.http = httptest.NewServer(f.master)
	return password
}

func localCLIGrant(t *testing.T, f *agentHTTPFixture, credential string, want int) string {
	t.Helper()
	r, err := http.NewRequest("POST", f.http.URL+"/api/agents/cli-access", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Agent-Key", credential)
	response, err := f.http.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("grant status %d, wanted %d", response.StatusCode, want)
	}
	token, _ := result["token"].(string)
	if want != http.StatusOK && token != "" {
		t.Fatal("failed grant exposed a token")
	}
	return token
}

func requireLocalCLIStatus(t *testing.T, f *agentHTTPFixture, token string, want int) {
	t.Helper()
	code, result := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes", token, nil)
	if code != want {
		t.Fatalf("local CLI status %d, wanted %d: %v", code, want, result)
	}
}

func localCLISessionCount(t *testing.T, f *agentHTTPFixture) int {
	t.Helper()
	count := 0
	if err := f.db.View(func(tx *state.Tx) error {
		return tx.Scan("sessions", "", "", 100, func(string, json.RawMessage) error { count++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestLocalCLIMasterAccessDoesNotChangeUserCredentials(t *testing.T) {
	f := &agentHTTPFixture{path: filepath.Join(t.TempDir(), "master.db")}
	t.Cleanup(f.close)
	initialPassword := openLocalCLIFixture(t, f)
	token := f.master.LocalCLIToken()
	if initialPassword == "" || token == "" {
		t.Fatal("missing initial credentials")
	}
	requireLocalCLIStatus(t, f, "", 401)
	requireLocalCLIStatus(t, f, token, 200)
	for _, authorization := range []string{token, "Basic " + token} {
		r, _ := http.NewRequest("GET", f.http.URL+"/api/auth/current", nil)
		r.Header.Set("Authorization", authorization)
		response, err := f.http.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("malformed authorization accepted", response.StatusCode)
		}
	}
	code, result := request(t, f.http.Client(), "GET", f.http.URL+"/api/auth/current", token, nil)
	user := result["user"].(map[string]any)
	if code != 200 || user["authentication"] != "local" || user["username"] != "local:master" || user["passwordHash"] != nil {
		t.Fatal("local identity was confused with a user account", code, result)
	}
	code, _ = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/network", token, map[string]string{"publicUrl": "https://local-cli.example"})
	if code != 200 {
		t.Fatal("local credential cannot administer its Master", code)
	}
	configuration, err := f.master.configuration()
	if err != nil || configuration.PublicURL != "https://local-cli.example" {
		t.Fatal("local administrative mutation did not persist", err)
	}
	code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/change-password", token, map[string]string{"oldPassword": initialPassword, "newPassword": "must not replace the account"})
	if code != 403 {
		t.Fatal("local identity changed another account's password", code)
	}
	code, result = request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/login", "", map[string]string{"username": "admin", "password": initialPassword})
	if code != 200 || result["user"].(map[string]any)["mustChangePassword"] != true {
		t.Fatal("local access changed browser login requirements", code, result)
	}
	loginToken := result["token"].(string)
	requireLocalCLIStatus(t, f, loginToken, 403)
	code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/logout", token, nil)
	if code != 200 {
		t.Fatal("local logout failed", code)
	}
	requireLocalCLIStatus(t, f, token, 200)
	f.close()
	if password := openLocalCLIFixture(t, f); password != "" || token != f.master.LocalCLIToken() {
		t.Fatal("restart rotated local grant or user credentials")
	}
	requireLocalCLIStatus(t, f, token, 200)
	requireLocalCLIStatus(t, f, loginToken, 403)
	other := newAgentHTTPFixture(t)
	other.close()
	openLocalCLIFixture(t, other)
	requireLocalCLIStatus(t, other, token, 401)
	requireLocalCLIStatus(t, f, other.master.LocalCLIToken(), 401)
}

func TestLocalCLIAgentGrantFollowsExactEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.close()
	openLocalCLIFixture(t, f)
	credential := agentCredential(t)
	sessionCount := localCLISessionCount(t, f)
	code, enrolled := f.join(t, f.invite(t, "cli-agent.example"), "cli-agent", credential)
	if code != 200 {
		t.Fatal("agent enrollment failed", code)
	}
	token := localCLIGrant(t, f, credential, 200)
	if token == "" || token == credential || token != localCLIGrant(t, f, credential, 200) {
		t.Fatal("grant must be separate, stable and idempotent")
	}
	requireLocalCLIStatus(t, f, token, 200)
	requireLocalCLIStatus(t, f, credential, 401)
	if code := f.agentStatus(t, "GET", "/api/master/nodes", credential, nil); code != 401 {
		t.Fatal("Agent transport key bypassed administrator authentication", code)
	}
	localCLIGrant(t, f, "", 401)
	localCLIGrant(t, f, agentCredential(t), 401)
	for _, malformed := range []string{"otcli1.", "otcli1." + strings.Repeat("x", 3000), token + "tampered", strings.Replace(token, "otcli1.", "otcli2.", 1), f.master.signLocalCLI(localCLIClaims{Role: "agent", NodeID: "missing", EnrollmentID: enrolled.EnrollmentID}), f.master.signLocalCLI(localCLIClaims{Role: "agent", NodeID: enrolled.NodeID, EnrollmentID: "wrong-enrollment"}), f.master.signLocalCLI(localCLIClaims{Role: "master", NodeID: enrolled.NodeID})} {
		requireLocalCLIStatus(t, f, malformed, 401)
	}
	code, _ = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/cli-agent.example", token, map[string]string{"label": "Updated using local CLI"})
	if code != 200 {
		t.Fatal("Agent local CLI cannot update its cluster", code)
	}
	f.close()
	openLocalCLIFixture(t, f)
	requireLocalCLIStatus(t, f, token, 200)
	if next := localCLIGrant(t, f, credential, 200); next != token {
		t.Fatal("Master restart changed Agent grant")
	}
	code, _ = request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/cli-agent.example", f.admin, nil)
	if code != 200 {
		t.Fatal("node revoke failed", code)
	}
	requireLocalCLIStatus(t, f, token, 401)
	localCLIGrant(t, f, credential, 401)
	newCredential := agentCredential(t)
	code, replacement := f.join(t, f.invite(t, "cli-agent.example"), "cli-agent", newCredential)
	if code != 200 || replacement.EnrollmentID == enrolled.EnrollmentID {
		t.Fatal("node re-enrollment failed", code)
	}
	requireLocalCLIStatus(t, f, token, 401)
	newToken := localCLIGrant(t, f, newCredential, 200)
	if token == newToken {
		t.Fatal("new enrollment revived old administrative grant")
	}
	requireLocalCLIStatus(t, f, newToken, 200)
	if count := localCLISessionCount(t, f); count != sessionCount {
		t.Fatalf("local CLI commands created login sessions: %d -> %d", sessionCount, count)
	}
}

func TestLocalCLIDisabledAndCorruptKeyFailClosed(t *testing.T) {
	f := newAgentHTTPFixture(t)
	if f.master.LocalCLIToken() != "" {
		t.Fatal("disabled local access produced a credential")
	}
	localCLIGrant(t, f, agentCredential(t), 404)
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("system", "local-cli-signing-key", []byte("corrupt")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(f.db, Options{LocalCLIEnabled: true}); err == nil {
		t.Fatal("corrupt local credential key silently replaced")
	}
}
