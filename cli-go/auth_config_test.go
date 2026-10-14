package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitLegacyTokenFailsBeforeSendingRequest(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "must-never-be-sent")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "valid-user-session")
	client := NewApiClient("http://example.test")
	client.client = &http.Client{Transport: apiTestTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("legacy token reached transport")
		return nil, nil
	})}
	_, err := client.Get("/status")
	if err == nil || !strings.Contains(err.Error(), "login") || strings.Contains(err.Error(), "must-never-be-sent") {
		t.Fatal("missing actionable redacted error", err)
	}
	if err = client.Download("GET", "/task-reports/1", nil, filepath.Join(t.TempDir(), "report.zip")); err == nil {
		t.Fatal("download bypassed legacy-token refusal")
	}
}
func TestCLIUsesSavedSessionDespiteLegacyDeploymentFile(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	config := filepath.Join(dir, "application.yml")
	if err := os.WriteFile(config, []byte("server.port: 1\ntransfer.internal-token: old-node-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", config)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Internal-Token") != "" {
			t.Error("node credential sent")
		}
		if r.Header.Get("Authorization") != "Bearer saved-user-session" {
			t.Error("saved login session not used")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"workers":{}}`)
	}))
	defer server.Close()
	if err := saveSession(server.URL, "saved-user-session"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", server.URL)
	var out, stderr bytes.Buffer
	if status := runCLI([]string{"--json", "node", "list"}, strings.NewReader(""), &out, &stderr); status != 0 || calls != 2 {
		t.Fatal(status, out.String(), stderr.String(), calls)
	}
	// A session is scoped to its exact origin, including its explicit port.
	if other, err := loadSession(server.URL + "0"); err != nil || other != "" {
		t.Fatal("session escaped origin", err)
	}
}
func TestImplicitLegacyYAMLNeverSuppliesAuthentication(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", "")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "application.yml"), []byte("transfer.internal-token: stale-local-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	client := NewApiClient("http://example.test")
	request, err := client.newRequest("GET", "/status", nil)
	if err != nil || request.Header.Get("X-Internal-Token") != "" || request.Header.Get("Authorization") != "" {
		t.Fatal("implicit deployment credential loaded", err)
	}
}
