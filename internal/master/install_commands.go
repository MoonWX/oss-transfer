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

const officialAgentArtifactBase = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer"
const legacyAgentArtifactBase = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer"

func (s *Server) agentArtifactBase() string {
	base := strings.TrimRight(s.options.AgentArtifactBase, "/")
	if base == legacyAgentArtifactBase {
		return officialAgentArtifactBase
	}
	return base
}

func (s *Server) regionalAgentArtifacts() bool {
	base := s.agentArtifactBase()
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

func bootstrapCommand(link string) string {
	return "curl -fsSL " + shellQuote(link) + " | bash"
}

func (s *Server) agentReleaseURL() string {
	if s.options.AgentArtifactBase == "" {
		return ""
	}
	return s.agentArtifactBase() + "/" + s.options.AgentArtifactVersion
}
func (s *Server) agentInvitationData(masterURL string, invite cluster.Invitation) map[string]any {
	if invite.MasterURL != "" {
		masterURL = invite.MasterURL
	}
	link := masterURL + "/api/agents/bootstrap/" + invite.Token
	data := map[string]any{"id": tokenKey(invite.Token), "expiresAt": invite.ExpiresAt, "inviteToken": invite.Token, "joinUrl": link, "nativeAgent": map[string]any{"available": false}, "nativeCommand": "./oss-transfer-agent --join-url " + shellQuote(link)}
	if s.agentReleaseURL() != "" {
		data["nativeAgent"] = map[string]any{"available": true}
		data["traditionalCommand"] = bootstrapCommand(link)
		data["windowsCommand"] = "irm " + psQuote(link+"?platform=windows") + " | iex"
	}
	if s.options.AgentDockerImage != "" {
		data["dockerCommand"] = bootstrapCommand(link + "?method=compose")
	}
	return data
}
func (s *Server) agentBootstrap(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Query().Get("method")
	windows := r.URL.Query().Get("platform") == "windows"
	if method != "" && method != "native" && method != "compose" || windows && method == "compose" {
		fail(w, http.StatusBadRequest, "Unsupported Agent installation method")
		return
	}
	if method == "compose" && s.options.AgentDockerImage == "" {
		fail(w, http.StatusServiceUnavailable, "Agent container image is not configured")
		return
	}
	if s.agentReleaseURL() == "" && method != "compose" {
		fail(w, 503, "Agent release artifacts are not configured")
		return
	}
	token := r.PathValue("token")
	invite, err := s.agents.registry.Invitation(token)
	if err != nil {
		fail(w, 404, "Invalid or expired Agent invitation")
		return
	}
	masterURL := invite.MasterURL
	if masterURL == "" {
		// Invitations created by an earlier version have no saved endpoint.
		config, err := s.configuration()
		if err != nil {
			fail(w, 500, "Cannot read Master address")
			return
		}
		masterURL = config.agentURL()
		if masterURL == "" {
			masterURL = requestOrigin(r)
		}
	}
	if !validMasterOrigin(masterURL) {
		fail(w, 400, "Master URL must be an HTTP or HTTPS origin")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.agentReleaseURL() == "" {
		s.containerAgentBootstrap(w, token, masterURL)
		return
	}
	// Keep version/image choices in the downloaded script. Older copied commands
	// can still pass their installer options through "$@".
	installerOptions := "--method native"
	if method == "compose" {
		installerOptions = "--method compose --image " + shellQuote(s.options.AgentDockerImage)
	}
	if s.regionalAgentArtifacts() {
		s.officialAgentBootstrap(w, windows, token, masterURL, installerOptions)
		return
	}
	if windows {
		fmt.Fprintf(w, "$ErrorActionPreference = 'Stop'\n$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')\ntry {\n  Invoke-WebRequest -UseBasicParsing -Uri %s -OutFile $installer\n  $env:TRANSFER_JOIN_TOKEN = %s\n  & ([scriptblock]::Create((Get-Content -LiteralPath $installer -Raw))) -Role agent -ReleaseUrl %s -MasterUrl %s -Yes\n} finally {\n  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue\n  Remove-Item $installer -ErrorAction SilentlyContinue\n}\n", psQuote(s.agentReleaseURL()+"/Install.ps1"), psQuote(token), psQuote(s.agentReleaseURL()), psQuote(masterURL))
		return
	}
	fmt.Fprintf(w, "#!/usr/bin/env bash\nset -euo pipefail\ninstaller=$(mktemp)\ntrap 'rm -f \"$installer\"' EXIT\ncurl -fSL %s -o \"$installer\"\nprintf '%%s' %s | bash \"$installer\" --role agent %s --release-url %s --master-url %s --token-stdin --yes \"$@\"\n", shellQuote(s.agentReleaseURL()+"/install.sh"), shellQuote(token), installerOptions, shellQuote(s.agentReleaseURL()), shellQuote(masterURL))
}

func (s *Server) containerAgentBootstrap(w http.ResponseWriter, token, masterURL string) {
	// Without release archives, retain the image-only installation path. The
	// running container uses its saved identity, never the invitation token.
	image := shellQuote(s.options.AgentDockerImage)
	fmt.Fprintf(w, `#!/usr/bin/env bash
set -euo pipefail
command -v docker >/dev/null || { printf 'Install Docker before joining this Agent.\n' >&2; exit 1; }
docker volume create oss-transfer-agent-data >/dev/null
export OSS_TRANSFER_JOIN_TOKEN=%s
trap 'unset OSS_TRANSFER_JOIN_TOKEN' EXIT
docker run --rm -e OSS_TRANSFER_JOIN_TOKEN -v oss-transfer-agent-data:/data %s oss-transfer-agent --master-url %s --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN
docker run -d --name oss-transfer-agent --restart unless-stopped -v oss-transfer-agent-data:/data %s oss-transfer-agent --data-dir /data
`, shellQuote(token), image, shellQuote(masterURL), image)
}

func (s *Server) officialAgentBootstrap(w http.ResponseWriter, windows bool, token, masterURL, installerOptions string) {
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
      Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $installer -TimeoutSec 60 -ErrorAction Stop
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
  & ([scriptblock]::Create((Get-Content -LiteralPath $installer -Raw))) -Role agent -Source auto -Version %s -MasterUrl %s -Yes
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
printf '%%s' %s | bash "$installer" --role agent %s --source auto --version %s --master-url %s --token-stdin --yes "$@"
`, shellQuote(github+"/install.sh"), shellQuote(jdoss+"/install.sh"), shellQuote(token), installerOptions, shellQuote(version), shellQuote(masterURL))
}
