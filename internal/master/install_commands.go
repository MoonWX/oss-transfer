package master

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

var artifactVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
var imageReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]+$`)
var artifactBase = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`)

const officialAgentArtifactBase = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer"

func (s *Server) regionalAgentArtifacts() bool {
	base := strings.TrimRight(s.options.AgentArtifactBase, "/")
	if base != officialAgentArtifactBase && base != "https://github.com/MoonWX/oss-transfer/releases/download" {
		return false
	}
	// Earlier pinned installers do not accept --source / -Source.
	version := strings.SplitN(strings.TrimPrefix(s.options.AgentArtifactVersion, "v"), "-", 2)[0]
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for i, minimum := range []int{4, 1, 6} {
		number, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		if number != minimum {
			return number > minimum
		}
	}
	return true
}

// ValidateAgentArtifacts also runs during installer preflight, before stopping a service.
func ValidateAgentArtifacts(options Options) error { return validateAgentArtifacts(options) }

func validateAgentArtifacts(options Options) error {
	if (options.AgentArtifactBase == "") != (options.AgentArtifactVersion == "") {
		return errors.New("Agent artifact base and version must be configured together")
	}
	if options.AgentArtifactBase != "" {
		// Match the URL forms supported by both native installers.
		if !artifactBase.MatchString(options.AgentArtifactBase) {
			return errors.New("Agent artifact URL requires a DNS or IPv4 host and an unescaped path")
		}
		u, err := url.Parse(options.AgentArtifactBase)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(options.AgentArtifactBase, "\r\n\x00") {
			return errors.New("invalid Agent artifact base URL")
		}
		ip := net.ParseIP(u.Hostname())
		loopback := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
		if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
			return errors.New("Agent artifacts require HTTPS")
		}
		if !artifactVersion.MatchString(options.AgentArtifactVersion) {
			return errors.New("Agent artifact version must name a fixed release")
		}
	}
	if options.AgentDockerImage != "" && !imageReference.MatchString(options.AgentDockerImage) {
		return errors.New("invalid Agent container image")
	}
	return nil
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func psQuote(value string) string    { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
func (s *Server) agentReleaseURL() string {
	if s.options.AgentArtifactBase == "" {
		return ""
	}
	return strings.TrimRight(s.options.AgentArtifactBase, "/") + "/" + s.options.AgentArtifactVersion
}
func (s *Server) agentInvitationData(masterURL string, invite cluster.Invitation) map[string]any {
	data := map[string]any{"id": tokenKey(invite.Token), "expiresAt": invite.ExpiresAt, "inviteToken": invite.Token, "joinUrl": masterURL + "/api/agents/join", "nativeAgent": map[string]any{"available": false}, "nativeCommand": "OSS_TRANSFER_JOIN_TOKEN=" + shellQuote(invite.Token) + " ./oss-transfer-agent --master-url " + shellQuote(masterURL) + " --data-dir ./agent-data"}
	if s.agentReleaseURL() != "" {
		link := masterURL + "/api/agents/bootstrap/" + invite.Token
		data["joinUrl"] = link
		data["nativeAgent"] = map[string]any{"available": true}
		data["traditionalCommand"] = "(umask 077; join_script=$(mktemp) || exit; trap 'rm -f \"$join_script\"' EXIT; curl -fSL " + shellQuote(link) + " -o \"$join_script\" && { if [ \"$(id -u)\" -eq 0 ]; then bash \"$join_script\"; else sudo bash \"$join_script\"; fi; })"
		data["windowsCommand"] = "$joinScript = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1'); try { Invoke-WebRequest -ErrorAction Stop -Uri " + psQuote(link+"?platform=windows") + " -OutFile $joinScript; & $joinScript } finally { Remove-Item $joinScript -ErrorAction SilentlyContinue }"
	}
	if s.options.AgentDockerImage != "" {
		// The one-time token is supplied to an enrollment container only. The
		// long-running container mounts the saved identity and contains no token.
		image := shellQuote(s.options.AgentDockerImage)
		data["dockerCommand"] = "docker volume create oss-transfer-agent-data &&\n" +
			"docker run --rm -e OSS_TRANSFER_JOIN_TOKEN=" + shellQuote(invite.Token) + " -v oss-transfer-agent-data:/data " + image + " oss-transfer-agent --master-url " + shellQuote(masterURL) + " --data-dir /data --enroll-only &&\n" +
			"docker run -d --name oss-transfer-agent --restart unless-stopped -v oss-transfer-agent-data:/data " + image + " oss-transfer-agent --data-dir /data"
	}
	return data
}
func (s *Server) agentBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.agentReleaseURL() == "" {
		fail(w, 503, "Agent release artifacts are not configured")
		return
	}
	token := r.PathValue("token")
	if err := s.agents.registry.CheckInvitation(token); err != nil {
		fail(w, 404, "Invalid or expired Agent invitation")
		return
	}
	config, err := s.configuration()
	if err != nil {
		fail(w, 500, "Cannot read Master address")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.regionalAgentArtifacts() {
		s.officialAgentBootstrap(w, r.URL.Query().Get("platform") == "windows", token, config.PublicURL)
		return
	}
	if r.URL.Query().Get("platform") == "windows" {
		fmt.Fprintf(w, "$ErrorActionPreference = 'Stop'\n$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')\ntry {\n  Invoke-WebRequest -Uri %s -OutFile $installer\n  $env:TRANSFER_JOIN_TOKEN = %s\n  & $installer -Role agent -ReleaseUrl %s -MasterUrl %s -Yes\n} finally {\n  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue\n  Remove-Item $installer -ErrorAction SilentlyContinue\n}\n", psQuote(s.agentReleaseURL()+"/Install.ps1"), psQuote(token), psQuote(s.agentReleaseURL()), psQuote(config.PublicURL))
		return
	}
	fmt.Fprintf(w, "#!/usr/bin/env bash\nset -euo pipefail\ninstaller=$(mktemp)\ntrap 'rm -f \"$installer\"' EXIT\ncurl -fSL %s -o \"$installer\"\nprintf '%%s' %s | bash \"$installer\" --role agent --method native --release-url %s --master-url %s --token-stdin --yes \"$@\"\n", shellQuote(s.agentReleaseURL()+"/install.sh"), shellQuote(token), shellQuote(s.agentReleaseURL()), shellQuote(config.PublicURL))
}

func (s *Server) officialAgentBootstrap(w http.ResponseWriter, windows bool, token, masterURL string) {
	version := "v" + strings.TrimPrefix(s.options.AgentArtifactVersion, "v")
	github := "https://raw.githubusercontent.com/MoonWX/oss-transfer/" + version
	jdoss := officialAgentArtifactBase + "/" + version
	if windows {
		fmt.Fprintf(w, `$ErrorActionPreference = 'Stop'
$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')
$source = 'github'
$country = ''
try {
  $trace = [string](Invoke-RestMethod -Uri 'https://www.cloudflare.com/cdn-cgi/trace' -TimeoutSec 3)
  if ($trace -match '(?m)^loc=([A-Z]{2})\r?$') { $country = $Matches[1] }
} catch {}
if ($country -notmatch '^[A-Z]{2}$' -or $country -eq 'XX') {
  try { $country = ([string](Invoke-RestMethod -Uri 'https://ipinfo.io/country' -TimeoutSec 3)).Trim().ToUpperInvariant() } catch {}
}
if ($country -eq 'CN') { $source = 'jdoss' }
$urls = @{ github = @(%s, %s); jdoss = @(%s) }
function Download-Installer($sourceName) {
  foreach ($url in $urls[$sourceName]) {
    try {
      Invoke-WebRequest -Uri $url -OutFile $installer -TimeoutSec 60 -ErrorAction Stop
      return
    } catch { $lastError = $_ }
  }
  throw $lastError
}
try {
  try {
    Download-Installer $source
  } catch {
    $source = if ($source -eq 'github') { 'jdoss' } else { 'github' }
    Download-Installer $source
  }
  $env:TRANSFER_JOIN_TOKEN = %s
  & $installer -Role agent -Source auto -Version %s -MasterUrl %s -Yes
} finally {
  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
  Remove-Item $installer -ErrorAction SilentlyContinue
}
`, psQuote(github+"/deploy/native/Install.ps1"), psQuote(github+"/deploy/go/Install.ps1"), psQuote(jdoss+"/Install.ps1"), psQuote(token), psQuote(version), psQuote(masterURL))
		return
	}
	fmt.Fprintf(w, `#!/usr/bin/env bash
set -euo pipefail
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
source=github
country=$(curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 --max-time 3 --max-filesize 4096 https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null | awk -F= '$1 == "loc" {sub(/\r$/, "", $2); print $2; exit}') || country=
if [[ ! "$country" =~ ^[A-Z]{2}$ || "$country" == XX ]]; then
  country=$(curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 --max-time 3 --max-filesize 4096 https://ipinfo.io/country 2>/dev/null | tr -d '\r\n') || country=
fi
[[ "$country" != CN ]] || source=jdoss
github_url=%s
jdoss_url=%s
if [[ "$source" == jdoss ]]; then url=$jdoss_url; else url=$github_url; fi
if ! curl -fSL --proto '=https' --proto-redir '=https' --connect-timeout 5 --max-time 60 "$url" -o "$installer"; then
  if [[ "$source" == jdoss ]]; then source=github; url=$github_url; else source=jdoss; url=$jdoss_url; fi
  curl -fSL --proto '=https' --proto-redir '=https' --connect-timeout 5 --max-time 60 "$url" -o "$installer"
fi
printf '%%s' %s | bash "$installer" --role agent --method native --source auto --version %s --master-url %s --token-stdin --yes "$@"
`, shellQuote(github+"/install.sh"), shellQuote(jdoss+"/install.sh"), shellQuote(token), shellQuote(version), shellQuote(masterURL))
}
