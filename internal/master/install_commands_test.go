package master

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

func TestAgentBootstrapExpiresAndCannotBeFetchedAfterEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = "https://downloads.example/osstransfer"
	f.master.options.AgentArtifactVersion = "v4.1.0"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:v4.1.0"
	invite, err := f.master.agents.registry.Invite("install-test", "Installer test")
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData("https://master.example:1685", invite)
	if !strings.Contains(data["traditionalCommand"].(string), "/api/agents/bootstrap/"+invite.Token) || !strings.Contains(data["windowsCommand"].(string), "platform=windows") {
		t.Fatal("bootstrap commands missing")
	}
	windows := data["windowsCommand"].(string)
	if !strings.Contains(windows, "Invoke-WebRequest -ErrorAction Stop") || !strings.Contains(windows, "[guid]::NewGuid()") || strings.Contains(windows, "./Join-Agent.ps1") {
		t.Fatal("Windows invitation can reuse a stale script after download failure")
	}
	docker := strings.Split(data["dockerCommand"].(string), "&&\n")
	if len(docker) != 3 || !strings.Contains(docker[1], "--enroll-only") || strings.Contains(docker[2], invite.Token) {
		t.Fatal("join token leaked into long-running container")
	}
	link := f.http.URL + "/api/agents/bootstrap/" + invite.Token
	for _, suffix := range []string{"", "?platform=windows"} {
		response, err := http.Get(link + suffix)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), "/v4.1.0/") || !strings.Contains(string(body), invite.Token) || strings.Contains(string(body), "/latest/") {
			t.Fatal(response.StatusCode, string(body))
		}
	}
	credential, _ := cluster.Token()
	if _, err = f.master.agents.registry.Join(cluster.JoinRequest{Token: invite.Token, InstanceID: "installed", Credential: credential}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("used bootstrap remains accessible", response.StatusCode)
	}
}

func TestAgentArtifactsMustNameFixedSafeRelease(t *testing.T) {
	for _, options := range []Options{
		{AgentArtifactBase: "https://downloads.example"},
		{AgentArtifactBase: "http://downloads.example", AgentArtifactVersion: "v4.1.0"},
		{AgentArtifactBase: "https://user:password@downloads.example", AgentArtifactVersion: "v4.1.0"},
		{AgentArtifactBase: "https://downloads.example", AgentArtifactVersion: "../latest"},
		{AgentArtifactBase: "https://downloads.example", AgentArtifactVersion: "latest"},
		{AgentArtifactBase: "https://downloads.example/path%20with%20spaces", AgentArtifactVersion: "v4.1.0"},
		{AgentArtifactBase: "https://[::1]/packages", AgentArtifactVersion: "v4.1.0"},
		{AgentDockerImage: "image; echo secret"},
	} {
		if err := validateAgentArtifacts(options); err == nil {
			t.Fatal("accepted unsafe artifact settings", options)
		}
	}
	if err := validateAgentArtifacts(Options{AgentArtifactBase: "http://127.0.0.1:1234", AgentArtifactVersion: "v4.1.0-test"}); err != nil {
		t.Fatal(err)
	}
	f := newAgentHTTPFixture(t)
	registry := cluster.New(f.db, func() time.Time { return time.Unix(100, 0) })
	invite, err := registry.Invite("expired", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = cluster.New(f.db, func() time.Time { return invite.ExpiresAt }).CheckInvitation(invite.Token); err == nil {
		t.Fatal("expired invitation accepted")
	}
}
