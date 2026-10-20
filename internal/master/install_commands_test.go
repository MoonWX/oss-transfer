package master

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

func TestAgentBootstrapExpiresAndCannotBeFetchedAfterEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = "https://downloads.example/osstransfer"
	f.master.options.AgentArtifactVersion = "v4.1.0"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.0"
	invite, err := f.master.agents.registry.Invite("install-test", "Installer test")
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData("https://master.example:1685", invite)
	if data["traditionalCommand"] != "curl -fsSL 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"' | bash" || !strings.Contains(data["windowsCommand"].(string), "platform=windows") {
		t.Fatal("bootstrap commands missing")
	}
	windows := data["windowsCommand"].(string)
	if windows != "irm 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"?platform=windows' | iex" {
		t.Fatal("Windows invitation must directly run the downloaded script")
	}
	docker := data["dockerCommand"].(string)
	if docker != "curl -fsSL 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"?method=compose' | bash" {
		t.Fatal("Docker enrollment must use the unified installer")
	}
	f.master.options.AgentDockerImage = "registry.example/transfer:custom"
	link := f.http.URL + "/api/agents/bootstrap/" + invite.Token
	for _, suffix := range []string{"", "?platform=windows", "?method=compose"} {
		response, err := http.Get(link + suffix)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), "/v4.1.0/") || !strings.Contains(string(body), invite.Token) || strings.Contains(string(body), "/latest/") {
			t.Fatal(response.StatusCode, string(body))
		}
		if suffix == "" && (!strings.Contains(string(body), "--method native") || !strings.Contains(string(body), "--token-stdin")) {
			t.Fatal("bootstrap must explicitly use native installation and stdin enrollment")
		}
		if suffix == "?method=compose" && !strings.Contains(string(body), "--method compose --image 'registry.example/transfer:custom'") {
			t.Fatal("bootstrap did not preserve the configured container image")
		}
	}
	credential, _ := cluster.Token()
	if _, err = f.master.agents.registry.Join(cluster.JoinRequest{Token: invite.Token, InstanceID: "installed", Credential: credential}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "?platform=windows", "?method=compose"} {
		response, err := http.Get(link + suffix)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("used bootstrap remains accessible", response.StatusCode)
		}
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

func TestOfficialAgentBootstrapSelectsSourceOnExecutionHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix Agent bootstrap requires Bash")
	}
	// Each case has its own invitation and download sandbox. Account setup is
	// shared because these cases exercise source selection, not password hashing.
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = officialAgentArtifactBase
	f.master.options.AgentArtifactVersion = "v4.1.6"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.6"
	for _, tc := range []struct {
		name, country, fallbackCountry, failSource, source, method string
		wantError, wantIPInfo, failBootstrap                       bool
	}{
		{name: "mainland", country: "CN", source: "jdoss"},
		{name: "international", country: "US", source: "github"},
		{name: "compose", country: "CN", source: "jdoss", method: "compose"},
		{name: "hong-kong", country: "HK", source: "github"},
		{name: "secondary-location", fallbackCountry: "CN", source: "jdoss", wantIPInfo: true},
		{name: "unknown-location", source: "github", wantIPInfo: true},
		{name: "invalid-location", country: "invalid", fallbackCountry: "invalid", source: "github", wantIPInfo: true},
		{name: "github-unreachable", country: "US", failSource: "github", source: "jdoss"},
		{name: "oss-unreachable", country: "CN", failSource: "jdoss", source: "github"},
		{name: "both-unreachable", country: "CN", failSource: "both", wantError: true},
		{name: "expired-invitation", failBootstrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invite, err := f.master.agents.registry.Invite("regional-bootstrap-"+tc.name, "")
			if err != nil {
				t.Fatal(err)
			}
			bootstrapURL := f.http.URL + "/api/agents/bootstrap/" + invite.Token
			if tc.method != "" {
				bootstrapURL += "?method=" + tc.method
			}
			response, err := http.Get(bootstrapURL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatal("bootstrap response", response.StatusCode, err)
			}
			dir := t.TempDir()
			stub := `#!/usr/bin/env bash
set -eu
url= out=
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --connect-timeout|--max-time) shift 2 ;;
    https://*|http://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[[ -z "${TRANSFER_JOIN_TOKEN:-}" ]]
if [[ "$url" == "$BOOTSTRAP_URL" ]]; then
  if [[ "$MOCK_FAIL_BOOTSTRAP" == true ]]; then
    printf 'curl: (22) The requested URL returned error: 404\n' >&2
    exit 22
  fi
  cat "$BOOTSTRAP_SCRIPT"; exit
fi
printf '%s\n' "$url" >> "$BOOTSTRAP_LOG"
case "$url" in
  https://www.cloudflare.com/cdn-cgi/trace) printf 'ip=203.0.113.1\nloc=%s\n' "$MOCK_COUNTRY" ;;
  https://ipinfo.io/country) printf '%s\n' "$MOCK_FALLBACK_COUNTRY" ;;
  https://raw.githubusercontent.com/MoonWX/oss-transfer/v4.1.6/install.sh) if [[ "$MOCK_FAIL_SOURCE" == github || "$MOCK_FAIL_SOURCE" == both ]]; then exit 22; fi; cp "$BOOTSTRAP_STUB" "$out" ;;
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v4.1.6/install.sh) if [[ "$MOCK_FAIL_SOURCE" == jdoss || "$MOCK_FAIL_SOURCE" == both ]]; then exit 22; fi; cp "$BOOTSTRAP_STUB" "$out" ;;
  *) exit 90 ;;
esac
`
			installer := "#!/usr/bin/env bash\nset -eu\nprintf '%s\\n' \"$@\" > \"$BOOTSTRAP_ARGS\"\ncat > \"$BOOTSTRAP_TOKEN\"\n"
			for name, contents := range map[string]string{"curl": stub, "sudo": "#!/usr/bin/env bash\nprintf 'sudo must not be needed by the bootstrap\\n' >&2\nexit 91\n", "installer.sh": installer, "join.sh": string(body)} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0700); err != nil {
					t.Fatal(err)
				}
			}
			invitation := f.master.agentInvitationData(f.http.URL, invite)
			commandText := invitation["traditionalCommand"].(string)
			if tc.method != "" {
				commandText = invitation["dockerCommand"].(string)
			}
			command := exec.Command("bash", "-c", commandText)
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TRANSFER_JOIN_TOKEN=", "MOCK_COUNTRY="+tc.country, "MOCK_FALLBACK_COUNTRY="+tc.fallbackCountry,
				"MOCK_FAIL_BOOTSTRAP="+strconv.FormatBool(tc.failBootstrap),
				"BOOTSTRAP_URL="+bootstrapURL, "BOOTSTRAP_SCRIPT="+filepath.Join(dir, "join.sh"),
				"MOCK_FAIL_SOURCE="+tc.failSource, "BOOTSTRAP_LOG="+filepath.Join(dir, "requests"),
				"BOOTSTRAP_STUB="+filepath.Join(dir, "installer.sh"), "BOOTSTRAP_ARGS="+filepath.Join(dir, "args"),
				"BOOTSTRAP_TOKEN="+filepath.Join(dir, "token"))
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("bootstrap execution: %v\n%s", err, output)
			}
			requests, _ := os.ReadFile(filepath.Join(dir, "requests"))
			if strings.Contains(string(requests), invite.Token) || strings.Contains(string(requests), "/latest/") || strings.Contains(string(requests), "/main/") {
				t.Fatal("bootstrap leaked invitation or fetched an unpinned installer")
			}
			if strings.Contains(string(requests), "https://ipinfo.io/country") != tc.wantIPInfo {
				t.Fatal("secondary lookup did not match missing country", string(requests))
			}
			args, argsErr := os.ReadFile(filepath.Join(dir, "args"))
			if tc.wantError || tc.failBootstrap {
				if !os.IsNotExist(argsErr) {
					t.Fatal("installer ran after a download failed")
				}
				if tc.failBootstrap && !strings.Contains(string(output), "curl: (22)") {
					t.Fatal("invitation download error was not shown", string(output))
				}
				return
			}
			if argsErr != nil || !strings.Contains(string(args), "--source\nauto\n--version\nv4.1.6\n") || strings.Contains(string(args), "--release-url") {
				t.Fatal("source/version not passed to installer", string(args), argsErr)
			}
			if tc.method != "" && !strings.Contains(string(args), "--method\n"+tc.method+"\n--image\nmoonwx/osstransfer:4.1.6\n") {
				t.Fatal("selected deployment method and pinned image did not reach unified installer", string(args))
			}
			requestLines := strings.Split(strings.TrimSpace(string(requests)), "\n")
			lastURL := requestLines[len(requestLines)-1]
			if (tc.source == "github") != strings.Contains(lastURL, "raw.githubusercontent.com") {
				t.Fatal("wrong regional installer downloaded", lastURL)
			}
			credential, err := os.ReadFile(filepath.Join(dir, "token"))
			if err != nil || string(credential) != invite.Token {
				t.Fatal("invitation was not passed to installer stdin", err)
			}
		})
	}
}

func TestAgentBootstrapKeepsCustomSourceAndWindowsVersion(t *testing.T) {
	for _, base := range []string{officialAgentArtifactBase, legacyAgentArtifactBase + "/", "https://downloads.example/osstransfer"} {
		f := newAgentHTTPFixture(t)
		f.master.options.AgentArtifactBase = base
		f.master.options.AgentArtifactVersion = "4.1.6"
		invite, err := f.master.agents.registry.Invite("bootstrap-source", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "?platform=windows"} {
			response, err := http.Get(f.http.URL + "/api/agents/bootstrap/" + invite.Token + suffix)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			text := string(body)
			if strings.HasPrefix(base, "https://downloads.example/") {
				if strings.Contains(text, "cloudflare.com") || strings.Contains(text, "ipinfo.io") || strings.Contains(text, "raw.githubusercontent.com") || !strings.Contains(text, base+"/4.1.6/") {
					t.Fatal("custom source was replaced or sent through location lookup", text)
				}
				continue
			}
			if !strings.Contains(text, officialAgentArtifactBase+"/v4.1.6/") || strings.Contains(text, legacyAgentArtifactBase+"/") {
				t.Fatal("official bootstrap did not use the renamed OSS prefix", text)
			}
			if strings.Contains(text, "--release-url") || strings.Contains(text, "-ReleaseUrl") || strings.Contains(text, "/latest/") {
				t.Fatal("official bootstrap does not select a fixed regional source", text)
			}
			if suffix != "" && (!strings.Contains(text, "-Source auto -Version 'v4.1.6'") || (!strings.Contains(text, "/v4.1.6/deploy/native/Install.ps1") || !strings.Contains(text, "/v4.1.6/deploy/go/Install.ps1")) || !strings.Contains(text, "/v4.1.6/Install.ps1") || !strings.Contains(text, "-TimeoutSec 3")) {
				t.Fatal("Windows source selection or tag pin missing", text)
			}
		}
	}
}

func TestAgentReleaseURLMigratesOnlyTheOldOfficialBase(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{legacyAgentArtifactBase, officialAgentArtifactBase},
		{legacyAgentArtifactBase + "/", officialAgentArtifactBase},
		{officialAgentArtifactBase, officialAgentArtifactBase},
		{legacyAgentArtifactBase + "-custom", legacyAgentArtifactBase + "-custom"},
		{"https://downloads.example/osstransfer", "https://downloads.example/osstransfer"},
	} {
		s := &Server{options: Options{AgentArtifactBase: tc.base, AgentArtifactVersion: "v4.1.5"}}
		if got := s.agentReleaseURL(); got != tc.want+"/v4.1.5" {
			t.Fatalf("Agent release URL for %s: %s", tc.base, got)
		}
	}
}

func TestAgentBootstrapKeepsOlderOfficialInstallerCompatible(t *testing.T) {
	for _, version := range []string{"v4.1.0", "4.1.5"} {
		f := newAgentHTTPFixture(t)
		f.master.options.AgentArtifactBase = officialAgentArtifactBase
		f.master.options.AgentArtifactVersion = version
		invite, err := f.master.agents.registry.Invite("older-bootstrap", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "?platform=windows"} {
			response, err := http.Get(f.http.URL + "/api/agents/bootstrap/" + invite.Token + suffix)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			text := string(body)
			if strings.Contains(text, "--source") || strings.Contains(text, "-Source") || strings.Contains(text, "cloudflare.com") || !strings.Contains(text, officialAgentArtifactBase+"/"+version+"/") {
				t.Fatal("older installer received unsupported source selection", text)
			}
		}
	}
}

func TestImageOnlyAgentBootstrapEnrollsBeforeStartingContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Container bootstrap requires Bash")
	}
	f := newAgentHTTPFixture(t)
	f.master.options.AgentDockerImage = "registry.example/transfer:custom"
	invite, err := f.master.agents.registry.InviteForMaster("container-bootstrap", "", f.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData(f.http.URL, invite)
	for _, failedEnrollment := range []bool{false, true} {
		t.Run(strconv.FormatBool(failedEnrollment), func(t *testing.T) {
			dir := t.TempDir()
			stub := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$DOCKER_CALLS"
case "$*" in
  *--enroll-only)
    [[ "${OSS_TRANSFER_JOIN_TOKEN:-}" == "$EXPECTED_TOKEN" ]]
    [[ "$FAIL_ENROLLMENT" != true ]] || exit 17
    ;;
  *) [[ -z "${OSS_TRANSFER_JOIN_TOKEN:-}" ]] ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", data["dockerCommand"].(string))
			command.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"OSS_TRANSFER_JOIN_TOKEN=", "EXPECTED_TOKEN="+invite.Token,
				"FAIL_ENROLLMENT="+strconv.FormatBool(failedEnrollment),
				"DOCKER_CALLS="+filepath.Join(dir, "calls"))
			output, err := command.CombinedOutput()
			if (err != nil) != failedEnrollment {
				t.Fatalf("container bootstrap: %v\n%s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			if strings.Contains(text, invite.Token) || strings.Contains(string(output), invite.Token) {
				t.Fatal("bootstrap exposed enrollment token in Docker arguments or output")
			}
			if !strings.Contains(text, "registry.example/transfer:custom oss-transfer-agent --master-url "+f.http.URL) {
				t.Fatal("enrollment lost custom image or Master endpoint", text)
			}
			if strings.Contains(text, "run -d") == failedEnrollment {
				t.Fatal("container must start only after successful enrollment", text)
			}
		})
	}
	for suffix, want := range map[string]int{
		"": 503, "?method=unknown": 400, "?method=compose&platform=windows": 400,
	} {
		response, err := http.Get(data["joinUrl"].(string) + suffix)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatal(suffix, response.StatusCode, want)
		}
	}
}

func TestWindowsAgentCommandRunsDownloadedInstaller(t *testing.T) {
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		shell, err = exec.LookPath("powershell")
	}
	if err != nil {
		t.Skip("PowerShell is not installed")
	}
	dir := t.TempDir()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4.1.11/Install.ps1" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `param($Role, $ReleaseUrl, $MasterUrl, [switch]$Yes)
[IO.File]::WriteAllText($env:INSTALLER_RESULT, "$Role|$ReleaseUrl|$MasterUrl|$env:TRANSFER_JOIN_TOKEN")
`)
	}))
	defer mirror.Close()
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = mirror.URL
	f.master.options.AgentArtifactVersion = "v4.1.11"
	invite, err := f.master.agents.registry.Invite("windows-bootstrap", "")
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData(f.http.URL, invite)
	result := filepath.Join(dir, "result")
	shellArgs := []string{"-NoProfile", "-NonInteractive"}
	if runtime.GOOS == "windows" {
		shellArgs = append(shellArgs, "-ExecutionPolicy", "Restricted")
	}
	command := exec.Command(shell, append(shellArgs, "-Command", data["windowsCommand"].(string))...)
	command.Env = append(os.Environ(), "INSTALLER_RESULT="+result)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell bootstrap: %v\n%s", err, output)
	}
	got, err := os.ReadFile(result)
	// Legacy invitations in this fixture inherit the configured Agent endpoint.
	want := "agent|" + mirror.URL + "/v4.1.11|https://master.example:1685|" + invite.Token
	if err != nil || string(got) != want {
		t.Fatal("Windows installer did not receive enrollment settings", err)
	}
}
