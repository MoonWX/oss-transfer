package master

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
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

// rootShell pipes the bootstrap to bash. Run by a user who is not root, the
// installer it downloads restarts itself through sudo.
const rootShell = ` | bash`

// bootstrapCommand runs the native installer as root.
func bootstrapCommand(link string) string {
	// curl 7.29 (CentOS 7) reads the brackets of an IPv6 literal as a glob.
	if u, err := url.Parse(link); err == nil && strings.HasPrefix(u.Host, "[") {
		return "curl -fsSL --globoff " + shellQuote(link) + rootShell
	}
	return "curl -fsSL " + shellQuote(link) + rootShell
}

// withQuery appends the non-empty name=value pairs to a bootstrap link.
func withQuery(link string, pairs ...string) string {
	query := []string{}
	for _, pair := range pairs {
		if pair != "" && !strings.HasSuffix(pair, "=") {
			query = append(query, pair)
		}
	}
	if len(query) == 0 {
		return link
	}
	return link + "?" + strings.Join(query, "&")
}

// installerLanguage maps the console or CLI language to the installers'
// --lang values. Japanese has no installer translation; the installer then
// follows the system locale.
func installerLanguage(locale string) string {
	switch locale {
	case "zh-CN":
		return "zh"
	case "en-US":
		return "en"
	}
	return ""
}

// languageEnv sets the installer language inside a bootstrap script. Older
// installers that predate --lang still read this environment variable.
func languageEnv(lang string, windows bool) string {
	if lang == "" {
		return ""
	}
	if windows {
		return "$env:OSS_TRANSFER_INSTALL_LANG = " + psQuote(lang) + "\n"
	}
	return "export OSS_TRANSFER_INSTALL_LANG=" + lang + "\n"
}

func requestInstallerLanguage(r *http.Request) string {
	locale, _ := r.Context().Value(responseLocaleKey{}).(string)
	return installerLanguage(locale)
}

// agentDockerCommands enrolls the Agent into a named volume, then starts the
// long-running container from the saved identity. Only docker itself is needed:
// no root, no host paths, and the same steps run on Docker Desktop and Linux.
// The names carry the invitation ID prefix so a second Agent on one host never
// reuses another Agent's volume, and re-running the same command after a failed
// enrollment retries with the same pending identity. The container's hostname
// is its name: the Agent reports it, so the console can show the commands that
// recreate this container with a new image.
func agentDockerCommands(image, link, invitationID string) (posix, powershell string) {
	name := "oss-transfer-agent-" + invitationID[:6]
	volume := name + "-data"
	hostAlias := ""
	if u, err := url.Parse(link); err == nil && strings.EqualFold(u.Hostname(), "host.docker.internal") {
		// Docker Engine on Linux defines this name only when asked to.
		hostAlias = " --add-host host.docker.internal:host-gateway"
	}
	enroll := func(quoted string) string {
		return "docker run --rm --hostname " + name + hostAlias + " -v " + volume + ":/data " + image + " oss-transfer-agent --join-url " + quoted + " --data-dir /data --enroll-only"
	}
	run := "docker run -d --name " + name + " --hostname " + name + " --restart unless-stopped" + hostAlias + " --read-only --tmpfs /tmp:mode=1777 --cap-drop ALL --security-opt no-new-privileges --stop-timeout 45 -v " + volume + ":/data " + image + " oss-transfer-agent --data-dir /data"
	posix = enroll(shellQuote(link)) + " && " + run
	// Windows PowerShell 5.1 has no &&; $LASTEXITCODE keeps the same order.
	powershell = enroll(psQuote(link)) + "; if ($LASTEXITCODE -eq 0) { " + run + " }"
	return posix, powershell
}

func (s *Server) agentReleaseURL() string {
	if s.options.AgentArtifactBase == "" {
		return ""
	}
	return s.agentArtifactBase() + "/" + s.options.AgentArtifactVersion
}

// agentDownloadSource tells the console where a native installer downloads
// its packages: from this Master, from GitHub or JD Cloud OSS chosen by the
// installing host's public IP location, or from a configured release URL.
func (s *Server) agentDownloadSource(upstream bool) map[string]any {
	if !upstream && s.cachedAgentPackages() {
		return map[string]any{"kind": "master"}
	}
	if s.agentReleaseURL() == "" {
		return nil
	}
	if s.regionalAgentArtifacts() {
		return map[string]any{"kind": "auto"}
	}
	return map[string]any{"kind": "release", "url": s.agentReleaseURL()}
}

func (s *Server) agentInvitationData(masterURL string, invite cluster.Invitation, lang string) map[string]any {
	if invite.MasterURL != "" {
		masterURL = invite.MasterURL
	}
	link := masterURL + "/api/agents/bootstrap/" + invite.Token
	id := tokenKey(invite.Token)
	language := "lang=" + lang
	data := map[string]any{"id": id, "expiresAt": invite.ExpiresAt, "inviteToken": invite.Token, "joinUrl": link, "nativeAgent": map[string]any{"available": false}, "nativeCommand": "./oss-transfer-agent --join-url " + shellQuote(link)}
	if s.agentReleaseURL() != "" || s.cachedAgentPackages() {
		data["nativeAgent"] = map[string]any{"available": true}
		data["traditionalCommand"] = bootstrapCommand(withQuery(link, language))
		data["windowsCommand"] = "irm " + psQuote(withQuery(link, "platform=windows", language)) + " | iex"
		if source := s.agentDownloadSource(false); source != nil {
			data["downloadSource"] = source
		}
	}
	if s.agentReleaseURL() != "" {
		data["upstreamTraditionalCommand"] = bootstrapCommand(withQuery(link, "source=upstream", language))
		data["upstreamWindowsCommand"] = "irm " + psQuote(withQuery(link, "platform=windows", "source=upstream", language)) + " | iex"
		if source := s.agentDownloadSource(true); source != nil {
			data["upstreamDownloadSource"] = source
		}
	}
	if s.options.AgentDockerImage != "" {
		data["dockerCommand"], data["dockerWindowsCommand"] = agentDockerCommands(s.options.AgentDockerImage, link, id)
		data["dockerImage"] = s.options.AgentDockerImage
		// The Compose installer manages /opt and /var/lib paths and needs root on Linux.
		data["composeCommand"] = bootstrapCommand(withQuery(link, "method=compose", language))
	}
	return data
}
func (s *Server) agentBootstrap(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Query().Get("method")
	source := r.URL.Query().Get("source")
	lang := r.URL.Query().Get("lang")
	windows := r.URL.Query().Get("platform") == "windows"
	if method != "" && method != "native" && method != "compose" || windows && method == "compose" || source != "" && source != "upstream" || lang != "" && lang != "en" && lang != "zh" {
		fail(w, http.StatusBadRequest, "Unsupported Agent installation method")
		return
	}
	if method == "compose" && s.options.AgentDockerImage == "" {
		fail(w, http.StatusServiceUnavailable, "Agent container image is not configured")
		return
	}
	if s.agentReleaseURL() == "" && method != "compose" && !s.cachedAgentPackages() {
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
	if method != "compose" && source != "upstream" {
		root, _, cacheErr := s.openAgentPackages()
		if cacheErr == nil {
			root.Close()
			s.masterAgentBootstrap(w, windows, token, masterURL, lang)
			return
		}
		if !errors.Is(cacheErr, os.ErrNotExist) {
			fail(w, http.StatusServiceUnavailable, "Master Agent packages are invalid")
			return
		}
	}
	if s.agentReleaseURL() == "" {
		if method != "compose" {
			fail(w, http.StatusServiceUnavailable, "Upstream Agent artifacts are not configured")
			return
		}
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
		s.officialAgentBootstrap(w, windows, token, masterURL, installerOptions, lang)
		return
	}
	if windows {
		fmt.Fprintf(w, "$ErrorActionPreference = 'Stop'\n%s$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')\ntry {\n  Invoke-WebRequest -UseBasicParsing -Uri %s -OutFile $installer\n  $env:TRANSFER_JOIN_TOKEN = %s\n  & ([scriptblock]::Create((Get-Content -LiteralPath $installer -Raw))) -Role agent -ReleaseUrl %s -MasterUrl %s -Yes\n} finally {\n  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue\n  Remove-Item $installer -ErrorAction SilentlyContinue\n}\n", languageEnv(lang, true), psQuote(s.agentReleaseURL()+"/Install.ps1"), psQuote(token), psQuote(s.agentReleaseURL()), psQuote(masterURL))
		return
	}
	fmt.Fprintf(w, "#!/usr/bin/env bash\nset -euo pipefail\n%sinstaller=$(mktemp)\ntrap 'rm -f \"$installer\"' EXIT\ncurl -fSL %s -o \"$installer\"\nprintf '%%s' %s | bash \"$installer\" --role agent %s --release-url %s --master-url %s --token-stdin --yes \"$@\"\n", languageEnv(lang, false), shellQuote(s.agentReleaseURL()+"/install.sh"), shellQuote(token), installerOptions, shellQuote(s.agentReleaseURL()), shellQuote(masterURL))
}

// All installer resources come from the same Master endpoint as enrollment.
// The invitation is consumed only after downloads and local checks complete.
func (s *Server) masterAgentBootstrap(w http.ResponseWriter, windows bool, token, masterURL, lang string) {
	releaseURL := masterURL + "/api/agents/bootstrap/" + token + "/files"
	if windows {
		fmt.Fprintf(w, `$ErrorActionPreference = 'Stop'
%s$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')
$release = %s
$architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
if ($architecture -notin @('amd64','arm64','x64')) { throw 'Unsupported Agent architecture.' }
if ($architecture -eq 'x64') { $architecture = 'amd64' }
try {
  Invoke-RestMethod -Uri ($release + '/manifest.tsv?target=windows-' + $architecture) | Out-Null
  Invoke-WebRequest -UseBasicParsing -Uri ($release + '/Install.ps1') -OutFile $installer
  $env:TRANSFER_JOIN_TOKEN = %s
  & ([scriptblock]::Create((Get-Content -LiteralPath $installer -Raw))) -Role agent -ReleaseUrl $release -MasterUrl %s -Yes
} finally {
  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue
  Remove-Item $installer -ErrorAction SilentlyContinue
}
`, languageEnv(lang, true), psQuote(releaseURL), psQuote(token), psQuote(masterURL))
		return
	}
	fmt.Fprintf(w, `#!/usr/bin/env bash
set -euo pipefail
%sinstaller=$(mktemp)
trap 'rm -f "$installer"' EXIT
release=%s
case "$(uname -s)" in Linux) platform=linux;; Darwin) platform=darwin;; FreeBSD) platform=freebsd;; *) echo 'Unsupported Agent system.' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; armv7l|armv7) arch=armv7;; riscv64) arch=riscv64;; loongarch64|loong64) arch=loong64;; *) echo 'Unsupported Agent architecture.' >&2; exit 1;; esac
curl -fsSL --globoff "$release/manifest.tsv?target=$platform-$arch" -o /dev/null
curl -fsSL --globoff "$release/install.sh" -o "$installer"
printf '%%s' %s | bash "$installer" --role agent --method native --release-url "$release" --master-url %s --token-stdin --yes "$@"
`, languageEnv(lang, false), shellQuote(releaseURL), shellQuote(token), shellQuote(masterURL))
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

func (s *Server) officialAgentBootstrap(w http.ResponseWriter, windows bool, token, masterURL, installerOptions, lang string) {
	version := "v" + strings.TrimPrefix(s.options.AgentArtifactVersion, "v")
	github := "https://raw.githubusercontent.com/MoonWX/oss-transfer/" + version
	jdoss := officialAgentArtifactBase + "/" + version
	if windows {
		fmt.Fprintf(w, `$ErrorActionPreference = 'Stop'
%s$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')
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
`, languageEnv(lang, true), psQuote(github+"/deploy/native/Install.ps1"), psQuote(github+"/deploy/go/Install.ps1"), psQuote(jdoss+"/Install.ps1"), psQuote(token), psQuote(version), psQuote(masterURL))
		return
	}
	fmt.Fprintf(w, `#!/usr/bin/env bash
set -euo pipefail
%sinstaller=$(mktemp)
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
`, languageEnv(lang, false), shellQuote(github+"/install.sh"), shellQuote(jdoss+"/install.sh"), shellQuote(token), installerOptions, shellQuote(version), shellQuote(masterURL))
}
