package master

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

var artifactVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
var imageReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]+$`)
var artifactBase = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`)

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
		data["traditionalCommand"] = "(umask 077; join_script=$(mktemp) || exit; trap 'rm -f \"$join_script\"' EXIT; curl -fSL " + shellQuote(link) + " -o \"$join_script\" && sudo bash \"$join_script\")"
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
	if r.URL.Query().Get("platform") == "windows" {
		fmt.Fprintf(w, "$ErrorActionPreference = 'Stop'\n$installer = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString() + '.ps1')\ntry {\n  Invoke-WebRequest -Uri %s -OutFile $installer\n  $env:TRANSFER_JOIN_TOKEN = %s\n  & $installer -Role agent -ReleaseUrl %s -MasterUrl %s -Yes\n} finally {\n  Remove-Item Env:TRANSFER_JOIN_TOKEN -ErrorAction SilentlyContinue\n  Remove-Item $installer -ErrorAction SilentlyContinue\n}\n", psQuote(s.agentReleaseURL()+"/Install.ps1"), psQuote(token), psQuote(s.agentReleaseURL()), psQuote(config.PublicURL))
		return
	}
	fmt.Fprintf(w, "#!/usr/bin/env bash\nset -euo pipefail\ninstaller=$(mktemp)\ntrap 'rm -f \"$installer\"' EXIT\ncurl -fSL %s -o \"$installer\"\nprintf '%%s' %s | bash \"$installer\" --role agent --release-url %s --master-url %s --token-stdin --yes \"$@\"\n", shellQuote(s.agentReleaseURL()+"/install.sh"), shellQuote(token), shellQuote(s.agentReleaseURL()), shellQuote(config.PublicURL))
}
