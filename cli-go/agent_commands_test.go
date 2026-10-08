package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentInvitationUsesConsoleAPI(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "invite", "--label", "storage-node", "--address", "10.0.0.2"},
		{"node", "add", "10.0.0.2", "--label", "storage-node"},
		{"node", "invite", "--label", "storage-node", "--address", "10.0.0.2"},
	} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/api/master/agents/invitations" {
				t.Fatalf("unexpected invitation request: %s %s", r.Method, r.URL)
			}
			body := adminTestBody(t, r)
			if body["address"] != "10.0.0.2" || body["label"] != "storage-node" || len(body) != 2 {
				t.Fatalf("unexpected invitation body: %#v", body)
			}
			adminTestJSON(t, w, map[string]interface{}{
				"success": true, "id": "test-invitation", "expiresAt": "2026-10-08T12:30:00Z",
				"joinUrl": "https://transfer.example/agent/join/once", "dockerCommand": "docker run moonwx/osstransfer agent --join-file ./join.json",
				"traditionalCommand": "sh ./agent-install.sh",
			})
		})
		result, err := dispatchCLI(client, args, strings.NewReader(""), "http://admin-cli-test.invalid")
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		renderCLIResult(&output, result)
		for _, wanted := range []string{"Agent 加入链接", "2026-10-08T12:30:00Z", "Docker", "docker run moonwx/osstransfer", "传统部署", "sh ./agent-install.sh"} {
			if !strings.Contains(output.String(), wanted) {
				t.Fatalf("invitation output missing %q: %s", wanted, output.String())
			}
		}
	}
}

func TestAgentInvitationAddressMayBeOmitted(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := adminTestBody(t, r)
		if body["address"] != "" || body["label"] != "storage-node" {
			t.Fatalf("unexpected auto-address invitation: %#v", body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	if _, err := runAdminCommand(client, "agent", []string{"invite", "--label", "storage-node"}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInvitationRejectsInvalidArgumentsWithoutHTTP(t *testing.T) {
	for _, args := range [][]string{
		{"invite", "extra"}, {"invite", "--address", "host.example"},
		{"invite", "--address", "10.0.0.2:22"}, {"invite", "--label", "first", "--label", "second"},
		{"invite", "--label", strings.Repeat("a", 81)}, {"invite", "--label", "a\nb"},
		{"join", "--token", "value"}, {"invite", "--address", ""},
	} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("invalid invitation arguments made a request: %v", args)
		})
		if _, err := runAdminCommand(client, "agent", args); err == nil {
			t.Errorf("invalid arguments accepted: %v", args)
		}
	}
	if watchable([]string{"agent", "invite"}) {
		t.Fatal("join token creation must never repeat with --watch")
	}
}

func TestAgentNodeStateUsesHeartbeat(t *testing.T) {
	result := map[string]interface{}{
		"data": map[string]interface{}{"nodes": []interface{}{
			map[string]interface{}{"address": "10.0.0.2", "label": "node-online", "agent": true, "online": true},
			map[string]interface{}{"address": "10.0.0.3", "label": "node-offline", "agent": true, "online": false},
		}},
	}
	var output bytes.Buffer
	renderCLIResult(&output, result)
	for _, wanted := range []string{"Agent", "node-online", "在线", "node-offline", "离线"} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("Agent state missing %q: %s", wanted, output.String())
		}
	}
}

func TestInternalTokenExplicitConfigFile(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	config := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(config, []byte("transfer.internal-token: 'test-config-token'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", config)
	if token := loadInternalToken(); token != "test-config-token" {
		t.Fatalf("explicit config token was not loaded: %q", token)
	}
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", config+".missing")
	if token := loadInternalToken(); token != "" {
		t.Fatal("explicit missing config must not read credentials from a different deployment")
	}
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "test-environment-token")
	if token := loadInternalToken(); token != "test-environment-token" {
		t.Fatal("environment token must take precedence over config")
	}
}

func TestAgentConfigurationNeverSendsLocalExecutorCredentialToMaster(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	config := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(config, []byte("transfer.internal-token: 'local-agent-executor-key'\ntransfer.role: 'agent' # local executor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", config)
	if token := loadInternalToken(); token != "" {
		t.Fatal("Agent local executor credential must not be used for Master administration")
	}
	client := NewApiClient("https://transfer.example.com")
	client.client = &http.Client{Transport: adminTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-Internal-Token") != "" {
			t.Fatal("Agent local executor credential was sent to the Master")
		}
		if request.Header.Get("Authorization") != "Bearer test-user-session" {
			t.Fatal("Agent administration should use the user's Master login session")
		}
		response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true}`))}
		return response, nil
	})}
	client.SetAuthToken("test-user-session")
	if _, err := client.Get("/nodes"); err != nil {
		t.Fatal(err)
	}
}
