package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testAgentJoinURL        = "https://transfer.example/api/agents/bootstrap/once"
	testAgentInstallCommand = "curl -fsSL '" + testAgentJoinURL + "' | sh -c 'if [ \"$(id -u)\" = 0 ]; then exec bash; else exec sudo bash; fi'"
	testAgentWindowsCommand = "irm '" + testAgentJoinURL + "?platform=windows' | iex"
	testAgentNativeCommand  = "./oss-transfer-agent --join-url '" + testAgentJoinURL + "'"
	testAgentDockerCommand  = "curl -fsSL '" + testAgentJoinURL + "?method=compose' | bash"
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
			if body["address"] != "10.0.0.2" || body["label"] != "storage-node" || len(body) != 3 || body["masterUrl"] != "http://"+r.Host {
				t.Fatalf("unexpected invitation body: %#v", body)
			}
			adminTestJSON(t, w, map[string]interface{}{
				"success": true, "id": "test-invitation", "expiresAt": "2026-10-08T12:30:00Z",
				"joinUrl": testAgentJoinURL, "dockerCommand": testAgentDockerCommand,
				"traditionalCommand": testAgentInstallCommand,
			})
		})
		result, err := dispatchCLI(client, args, strings.NewReader(""), "http://admin-cli-test.invalid")
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		renderCLIResult(&output, result)
		for _, wanted := range []string{invitationExpiry("2026-10-08T12:30:00Z"), "Docker", testAgentDockerCommand, "原生部署", testAgentInstallCommand} {
			if !strings.Contains(output.String(), wanted) {
				t.Fatalf("invitation output missing %q: %s", wanted, output.String())
			}
		}
	}
}

func TestAgentInvitationShowsNativeCommandsBeforeOptionalDocker(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		t.Run(string(locale), func(t *testing.T) {
			data := map[string]interface{}{
				"joinUrl":            testAgentJoinURL,
				"traditionalCommand": testAgentInstallCommand,
				"windowsCommand":     testAgentWindowsCommand,
				"nativeCommand":      testAgentNativeCommand,
				"dockerCommand":      testAgentDockerCommand,
			}
			var output bytes.Buffer
			renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, data)
			text := output.String()
			previous := -1
			for _, key := range []string{"traditionalCommand", "windowsCommand", "dockerCommand"} {
				position := strings.Index(text, data[key].(string))
				if position <= previous {
					t.Fatalf("native commands are missing or out of order: %s", text)
				}
				previous = position
			}
			if !strings.Contains(text, locale.text("\nDocker（可选）")) || strings.Contains(text, data["nativeCommand"].(string)) {
				t.Fatalf("optional Docker label missing or redundant native fallback: %s", text)
			}

			delete(data, "dockerCommand")
			output.Reset()
			renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, data)
			text = output.String()
			if !strings.Contains(text, locale.text("\n原生部署（Linux / macOS / FreeBSD，需要 root 或 sudo）")) || !strings.Contains(text, data["windowsCommand"].(string)) || strings.Contains(text, "Docker") || strings.Contains(text, "<nil>") {
				t.Fatalf("native-only invitation does not render cleanly: %s", text)
			}
		})
	}
}

func TestAgentInvitationShowsOnlyAvailableCommands(t *testing.T) {
	for _, test := range []struct{ field, command, label string }{
		{"nativeCommand", testAgentNativeCommand, "原生运行"},
		{"windowsCommand", testAgentWindowsCommand, "Windows PowerShell"},
		{"dockerCommand", testAgentDockerCommand, "Docker（可选）"},
	} {
		t.Run(test.field, func(t *testing.T) {
			var output bytes.Buffer
			renderCLIResult(&output, map[string]interface{}{
				"joinUrl": "https://transfer.example/api/agents/join", test.field: test.command,
			})
			text := output.String()
			if !strings.Contains(text, test.label) || !strings.Contains(text, test.command) || strings.Contains(text, "<nil>") {
				t.Fatalf("available command not rendered: %s", text)
			}
		})
	}
}

func TestAgentInvitationDoesNotRepeatJoinURL(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		t.Run(string(locale), func(t *testing.T) {
			for _, availability := range []interface{}{nil, map[string]interface{}{"available": true}, map[string]interface{}{"available": false}} {
				data := map[string]interface{}{
					"joinUrl":       testAgentJoinURL,
					"nativeAgent":   availability,
					"nativeCommand": testAgentNativeCommand,
					"expiresAt":     "2026-10-08T12:30:00Z",
				}
				var output bytes.Buffer
				renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, data)
				text := output.String()
				if strings.Count(text, testAgentJoinURL) != 1 {
					t.Fatalf("join URL should appear only in the command: %s", text)
				}
				if !strings.Contains(text, testAgentNativeCommand) || !strings.Contains(text, invitationExpiry(data["expiresAt"].(string))) {
					t.Fatalf("native fallback command or expiry missing: %s", text)
				}
			}
		})
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
		{"invite", "extra"}, {"invite", "--address", "host/name"},
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

func TestAgentConfigurationNeverSendsLocalExecutorCredentialToMaster(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	config := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(config, []byte("transfer.internal-token: 'local-agent-executor-key'\ntransfer.role: 'agent' # local executor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_CONFIG_FILE", config)
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

func TestAgentInvitationExplicitMasterOrigin(t *testing.T) {
	// node add accepts --master-url too: the loopback warning names it.
	invite := map[string]func(*ApiClient, []string) (map[string]interface{}, error){
		"agent invite": func(client *ApiClient, args []string) (map[string]interface{}, error) {
			return runAgentAdmin(client, "invite", args)
		},
		"node add": func(client *ApiClient, args []string) (map[string]interface{}, error) {
			return runAdminCommand(client, "node", append([]string{"add", "10.0.0.9"}, args...))
		},
	}
	for name, run := range invite {
		for _, origin := range []string{"https://transfer.example", "http://10.0.0.2:1685", "https://[2001:db8::1]:8443"} {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if adminTestBody(t, r)["masterUrl"] != origin {
					t.Fatal(name, "Master endpoint override was lost")
				}
				adminTestJSON(t, w, map[string]any{"success": true})
			})
			if _, err := run(client, []string{"--master-url", origin + "/"}); err != nil {
				t.Fatal(name, err)
			}
		}
		for _, origin := range []string{"", "ftp://host", "https://host/path", "https://host:0", "https://host:65536", "https://user:secret@host"} {
			client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal(name, "invalid origin submitted") })
			if _, err := run(client, []string{"--master-url", origin}); err == nil {
				t.Fatal(name, "invalid origin accepted", origin)
			}
		}
	}
}

func TestAgentInvitationWarnsAboutLoopbackForNativeCommands(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		var output bytes.Buffer
		renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, map[string]interface{}{
			"joinUrl": "http://127.0.0.1:1685/api/agents/bootstrap/once", "traditionalCommand": testAgentInstallCommand})
		if !strings.Contains(output.String(), locale.text("注意：Master 地址是本机回环地址，只有在 Master 主机上原生运行的 Agent 能访问。其他主机上的 Agent 请用 --master-url 指定局域网地址；Master 主机上的 Docker 容器请指定 host.docker.internal。")) {
			t.Fatalf("%s: loopback warning missing for a native command: %s", locale, output.String())
		}
	}
}

func TestAgentInvitationShowsLocalExpiryDockerVariantsAndDownloadSource(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("CST", 8*3600)
	t.Cleanup(func() { time.Local = previous })
	dockerCommand := "docker run --rm -v oss-transfer-agent-abcdef-data:/data moonwx/osstransfer:4.2.0 oss-transfer-agent --join-url '" + testAgentJoinURL + "' --data-dir /data --enroll-only && docker run -d --name oss-transfer-agent-abcdef moonwx/osstransfer:4.2.0 oss-transfer-agent --data-dir /data"
	windowsDocker := "docker run --rm oss-transfer-agent --enroll-only; if ($LASTEXITCODE -eq 0) { docker run -d moonwx/osstransfer:4.2.0 }"
	composeCommand := "curl -fsSL '" + testAgentJoinURL + "?method=compose' | sh -c 'if [ \"$(id -u)\" = 0 ]; then exec bash; else exec sudo bash; fi'"
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		t.Run(string(locale), func(t *testing.T) {
			data := map[string]interface{}{
				"joinUrl":              testAgentJoinURL,
				"expiresAt":            "2026-11-09T16:24:40.091547292Z",
				"traditionalCommand":   testAgentInstallCommand,
				"windowsCommand":       testAgentWindowsCommand,
				"downloadSource":       map[string]interface{}{"kind": "auto"},
				"dockerCommand":        dockerCommand,
				"dockerWindowsCommand": windowsDocker,
				"composeCommand":       composeCommand,
			}
			var output bytes.Buffer
			renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, data)
			text := output.String()
			if strings.Contains(text, "2026-11-09T16:24:40") || !strings.Contains(text, "2026-11-10 00:24:40 (UTC+08:00)") {
				t.Fatalf("expiry must be shown in local time: %s", text)
			}
			previous := -1
			for _, wanted := range []string{
				locale.text("\n原生部署（Linux / macOS / FreeBSD，需要 root 或 sudo）"), testAgentInstallCommand, testAgentWindowsCommand,
				locale.text("下载来源：GitHub 或京东云 OSS，按新节点公网 IP 所在地自动选择（会访问 cloudflare.com 和 ipinfo.io）"),
				locale.text("\nDocker（macOS / Linux，无需 root）"), dockerCommand,
				locale.text("\nDocker（Windows PowerShell）"), windowsDocker,
				locale.text("\nDocker Compose（Linux，需要 root 或 sudo）"), composeCommand,
			} {
				position := strings.Index(text, wanted)
				if position <= previous {
					t.Fatalf("missing or out of order %q:\n%s", wanted, text)
				}
				previous = position
			}
			if strings.Contains(text, locale.text("\nDocker（可选）")) || strings.Contains(text, "回环") || strings.Contains(text, "loopback") {
				t.Fatalf("unexpected legacy label or loopback warning: %s", text)
			}
			data["joinUrl"] = "http://localhost:1685/api/agents/bootstrap/once"
			output.Reset()
			renderCLIResult(&cliLocaleWriter{Writer: &output, locale: locale}, data)
			if !strings.Contains(output.String(), locale.text("注意：Master 地址是本机回环地址，只有在 Master 主机上原生运行的 Agent 能访问。其他主机上的 Agent 请用 --master-url 指定局域网地址；Master 主机上的 Docker 容器请指定 host.docker.internal。")) {
				t.Fatalf("loopback warning missing: %s", output.String())
			}
		})
	}
	for source, want := range map[string]string{"master": "下载来源：本 Master", "release": "下载来源：https://downloads.example/v4.2.0"} {
		var output bytes.Buffer
		renderCLIResult(&output, map[string]interface{}{"joinUrl": testAgentJoinURL, "traditionalCommand": testAgentInstallCommand,
			"downloadSource": map[string]interface{}{"kind": source, "url": "https://downloads.example/v4.2.0"}})
		if !strings.Contains(output.String(), want) {
			t.Fatalf("download source %s: %s", source, output.String())
		}
	}
}
