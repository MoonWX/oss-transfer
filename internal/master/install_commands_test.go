package master

import (
	"encoding/json"
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

func TestBootstrapCommandDisablesURLGlobbingForIPv6Masters(t *testing.T) {
	// curl 7.29 (CentOS 7) would read the brackets as a glob range. The native
	// installer needs root, so the copied command runs it as root: directly for
	// root, with sudo otherwise.
	for link, want := range map[string]string{
		"http://[fd00::43]:1685/api/agents/bootstrap/token": "curl -fsSL --globoff 'http://[fd00::43]:1685/api/agents/bootstrap/token'" + rootShell,
		"http://10.1.0.43:1685/api/agents/bootstrap/token":  "curl -fsSL 'http://10.1.0.43:1685/api/agents/bootstrap/token'" + rootShell,
		"https://master.example/api/agents/bootstrap/token": "curl -fsSL 'https://master.example/api/agents/bootstrap/token'" + rootShell,
	} {
		if got := bootstrapCommand(link); got != want {
			t.Errorf("bootstrapCommand(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestAgentBootstrapExpiresAndCannotBeFetchedAfterEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = "https://downloads.example/osstransfer"
	f.master.options.AgentArtifactVersion = "v4.1.0"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.0"
	invite, err := f.master.agents.registry.Invite("install-test", "Installer test")
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData("https://master.example:1685", invite, "")
	if data["traditionalCommand"] != "curl -fsSL 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"'"+rootShell || !strings.Contains(data["windowsCommand"].(string), "platform=windows") {
		t.Fatal("bootstrap commands missing")
	}
	windows := data["windowsCommand"].(string)
	if windows != "irm 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"?platform=windows' | iex" {
		t.Fatal("Windows invitation must directly run the downloaded script")
	}
	if compose := data["composeCommand"].(string); compose != "curl -fsSL 'https://master.example:1685/api/agents/bootstrap/"+invite.Token+"?method=compose'"+rootShell {
		t.Fatal("Linux Compose enrollment must use the unified installer as root", compose)
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
		wantError, wantIPInfo, failBootstrap, root                 bool
	}{
		{name: "mainland", country: "CN", source: "jdoss"},
		// FreeBSD's base system and minimal images run as root without sudo.
		{name: "root-without-sudo", country: "US", source: "github", root: true},
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
			sudo := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$SUDO_CALLS\"\nexec \"$@\"\n"
			uid := "1000"
			if tc.root {
				sudo, uid = "#!/bin/sh\necho 'sudo: command not found' >&2\nexit 127\n", "0"
			}
			id := "#!/bin/sh\nif [ \"$1\" = -u ]; then echo " + uid + "; else exec /usr/bin/id \"$@\"; fi\n"
			for name, contents := range map[string]string{"curl": stub, "sudo": sudo, "id": id, "installer.sh": installer, "join.sh": string(body)} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0700); err != nil {
					t.Fatal(err)
				}
			}
			invitation := f.master.agentInvitationData(f.http.URL, invite, "")
			commandText := invitation["traditionalCommand"].(string)
			if tc.method != "" {
				commandText = invitation["composeCommand"].(string)
			}
			command := exec.Command("bash", "-c", commandText)
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TRANSFER_JOIN_TOKEN=", "MOCK_COUNTRY="+tc.country, "MOCK_FALLBACK_COUNTRY="+tc.fallbackCountry,
				"MOCK_FAIL_BOOTSTRAP="+strconv.FormatBool(tc.failBootstrap),
				"BOOTSTRAP_URL="+bootstrapURL, "BOOTSTRAP_SCRIPT="+filepath.Join(dir, "join.sh"),
				"MOCK_FAIL_SOURCE="+tc.failSource, "BOOTSTRAP_LOG="+filepath.Join(dir, "requests"),
				"BOOTSTRAP_STUB="+filepath.Join(dir, "installer.sh"), "BOOTSTRAP_ARGS="+filepath.Join(dir, "args"),
				"BOOTSTRAP_TOKEN="+filepath.Join(dir, "token"), "SUDO_CALLS="+filepath.Join(dir, "sudo-calls"))
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("bootstrap execution: %v\n%s", err, output)
			}
			// The copied command pipes to plain bash and never calls sudo; a
			// user who is not root is told by the installer to use sudo.
			if sudo, _ := os.ReadFile(filepath.Join(dir, "sudo-calls")); len(sudo) != 0 {
				t.Fatalf("copied command called sudo: %q", sudo)
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
	data := f.master.agentInvitationData(f.http.URL, invite, "")
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
			if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/usr/bin/env bash\nexec \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", data["composeCommand"].(string))
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
	data := f.master.agentInvitationData(f.http.URL, invite, "")
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

func TestAgentDockerCommandsNeedOnlyDockerAndStartAfterEnrollment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Docker command requires a POSIX shell")
	}
	f := newAgentHTTPFixture(t)
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.41"
	invite, err := f.master.agents.registry.InviteForMaster("docker-desktop", "", "http://host.docker.internal:1685")
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.master.agents.registry.InviteForMaster("docker-second", "", "http://10.1.0.43:1685")
	if err != nil {
		t.Fatal(err)
	}
	data := f.master.agentInvitationData(f.http.URL, invite, "")
	second := f.master.agentInvitationData(f.http.URL, other, "")
	name := "oss-transfer-agent-" + data["id"].(string)[:6]
	link := "http://host.docker.internal:1685/api/agents/bootstrap/" + invite.Token
	for _, key := range []string{"dockerCommand", "dockerWindowsCommand"} {
		command := data[key].(string)
		// Docker Desktop on macOS and Windows: no root, no installer, no host paths.
		for _, forbidden := range []string{"sudo", "curl", "/opt", "/var/lib", "bash", "compose"} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("%s contains %q: %s", key, forbidden, command)
			}
		}
		if strings.Count(command, "--add-host host.docker.internal:host-gateway") != 2 || strings.Count(command, " -v "+name+"-data:/data ") != 2 || strings.Count(command, " --hostname "+name+" ") != 2 {
			t.Fatalf("%s must reach the host Master and share one named volume: %s", key, command)
		}
		if strings.Contains(second[key].(string), name) || strings.Contains(second[key].(string), "--add-host") {
			t.Fatalf("a second invitation must use its own names and no host alias: %s", second[key])
		}
	}
	if !strings.Contains(data["dockerWindowsCommand"].(string), "--join-url '"+link+"' --data-dir /data --enroll-only; if ($LASTEXITCODE -eq 0) { docker run -d --name "+name+" ") {
		t.Fatal("PowerShell command must start the Agent only after enrollment succeeds", data["dockerWindowsCommand"])
	}
	for _, failedEnrollment := range []bool{false, true} {
		t.Run("enrollment-failed="+strconv.FormatBool(failedEnrollment), func(t *testing.T) {
			dir := t.TempDir()
			stub := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$DOCKER_CALLS"
case "$*" in *--enroll-only) [[ "$FAIL_ENROLLMENT" != true ]] || exit 17 ;; esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", data["dockerCommand"].(string))
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAIL_ENROLLMENT="+strconv.FormatBool(failedEnrollment), "DOCKER_CALLS="+filepath.Join(dir, "calls"))
			output, err := command.CombinedOutput()
			if (err != nil) != failedEnrollment {
				t.Fatalf("docker command: %v\n%s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			want := "run --rm --hostname " + name + " --add-host host.docker.internal:host-gateway -v " + name + "-data:/data moonwx/osstransfer:4.1.41 oss-transfer-agent --join-url " + link + " --data-dir /data --enroll-only\n"
			if !failedEnrollment {
				want += "run -d --name " + name + " --hostname " + name + " --restart unless-stopped --add-host host.docker.internal:host-gateway --read-only --tmpfs /tmp:mode=1777 --cap-drop ALL --security-opt no-new-privileges --stop-timeout 45 -v " + name + "-data:/data moonwx/osstransfer:4.1.41 oss-transfer-agent --data-dir /data\n"
			}
			if string(calls) != want {
				t.Fatalf("docker calls:\n%s\nwant:\n%s", calls, want)
			}
		})
	}
	if shell, err := exec.LookPath("pwsh"); err == nil {
		dir := t.TempDir()
		stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CALLS\"\ncase \"$*\" in *--enroll-only) exit 17 ;; esac\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", data["dockerWindowsCommand"].(string))
		command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_CALLS="+filepath.Join(dir, "calls"))
		_ = command.Run()
		calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
		if strings.Contains(string(calls), "run -d") || !strings.Contains(string(calls), "--enroll-only") {
			t.Fatal("PowerShell command started the Agent after a failed enrollment", string(calls))
		}
	}
}

func TestAgentInvitationPassesConsoleLanguageToInstaller(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = officialAgentArtifactBase
	f.master.options.AgentArtifactVersion = "v4.1.41"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.41"
	create := func(language string) map[string]any {
		t.Helper()
		body := strings.NewReader(`{"label":"language-check","masterUrl":"http://10.1.0.43:1685"}`)
		request, err := http.NewRequest("POST", f.http.URL+"/api/master/agents/invitations", body)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.admin)
		request.Header.Set("Content-Type", "application/json")
		if language != "" {
			request.Header.Set("Accept-Language", language)
		}
		response, err := f.http.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil || response.StatusCode != 200 {
			t.Fatal(response.StatusCode, err)
		}
		return result["data"].(map[string]any)
	}
	for language, want := range map[string]string{"zh-CN": "lang=zh", "en-US,en;q=0.9": "lang=en", "ja-JP": "", "": ""} {
		data := create(language)
		for _, key := range []string{"traditionalCommand", "windowsCommand", "upstreamTraditionalCommand", "upstreamWindowsCommand", "composeCommand"} {
			command := data[key].(string)
			if want == "" && strings.Contains(command, "lang=") || want != "" && !strings.Contains(command, want) {
				t.Fatalf("%s for %q: %s", key, language, command)
			}
		}
		if strings.Contains(data["dockerCommand"].(string), "lang=") {
			t.Fatal("plain Docker commands have no installer to translate", data["dockerCommand"])
		}
	}
	link := create("zh-CN")["joinUrl"].(string)
	link = f.http.URL + link[strings.Index(link, "/api/"):]
	for suffix, want := range map[string]string{
		"?lang=zh":                  "\nexport OSS_TRANSFER_INSTALL_LANG=zh\n",
		"?method=compose&lang=zh":   "\nexport OSS_TRANSFER_INSTALL_LANG=zh\n",
		"?platform=windows&lang=zh": "\n$env:OSS_TRANSFER_INSTALL_LANG = 'zh'\n",
		"?source=upstream&lang=en":  "\nexport OSS_TRANSFER_INSTALL_LANG=en\n",
		"?platform=windows":         "",
		"":                          "",
	} {
		response, err := http.Get(link + suffix)
		if err != nil {
			t.Fatal(err)
		}
		script, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || want != "" && !strings.Contains(string(script), want) || want == "" && strings.Contains(string(script), "OSS_TRANSFER_INSTALL_LANG") {
			t.Fatalf("bootstrap %q: HTTP %d\n%s", suffix, response.StatusCode, script)
		}
	}
	response, err := http.Get(link + "?lang=fr")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal("unsupported installer language accepted", response.StatusCode)
	}
}

func TestAgentInvitationNamesItsDownloadSource(t *testing.T) {
	for _, tc := range []struct {
		base, version string
		want          map[string]any
	}{
		{officialAgentArtifactBase, "v4.1.41", map[string]any{"kind": "auto"}},
		{"https://downloads.example/osstransfer", "v4.1.41", map[string]any{"kind": "release", "url": "https://downloads.example/osstransfer/v4.1.41"}},
		// Older official installers cannot choose a regional source.
		{officialAgentArtifactBase, "v4.1.5", map[string]any{"kind": "release", "url": officialAgentArtifactBase + "/v4.1.5"}},
	} {
		s := &Server{options: Options{AgentArtifactBase: tc.base, AgentArtifactVersion: tc.version, DataDirectory: t.TempDir()}}
		data := s.agentInvitationData("http://10.1.0.43:1685", cluster.Invitation{Token: "token-for-source-check"}, "")
		for _, key := range []string{"downloadSource", "upstreamDownloadSource"} {
			got, _ := data[key].(map[string]any)
			if len(got) != len(tc.want) || got["kind"] != tc.want["kind"] || got["url"] != tc.want["url"] {
				t.Fatalf("%s %s %s = %#v, want %#v", tc.base, tc.version, key, data[key], tc.want)
			}
		}
	}
	s := &Server{options: Options{DataDirectory: t.TempDir()}}
	data := s.agentInvitationData("http://10.1.0.43:1685", cluster.Invitation{Token: "token-for-source-check"}, "")
	if _, ok := data["downloadSource"]; ok {
		t.Fatal("download source shown without native packages", data)
	}
}
