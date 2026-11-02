package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

const agentPackageLimit int64 = 1 << 30

var packageHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var packageCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}(-dirty)?$`)
var runningPackageCommitPattern = regexp.MustCompile(`^[a-f0-9]{7,40}(-dirty)?$`)
var packageNamePattern = regexp.MustCompile(`^oss-transfer-(linux|darwin|windows|freebsd)-(amd64|arm64|armv7|386|riscv64|loong64|ppc64le|s390x)\.(tar\.gz|zip)$`)

type agentPackage struct {
	name, checksum string
	size           int64
}
type agentPackageManifest struct {
	version, commit string
	files           map[string]agentPackage
	records         map[string]string
	raw             []byte
}

// A manifest is installed with the Master, so an invitation never chooses a
// download origin, version, filesystem path or checksum.
func parseAgentPackageManifest(raw []byte) (agentPackageManifest, error) {
	m := agentPackageManifest{raw: raw, files: make(map[string]agentPackage), records: make(map[string]string)}
	if len(raw) == 0 || len(raw) >= 65536 || strings.ContainsRune(string(raw), '\r') {
		return m, errors.New("invalid package manifest")
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		parts := strings.Split(line, "\t")
		var name, checksum, length string
		switch parts[0] {
		case "format", "version", "commit":
			if len(parts) != 2 || seen[parts[0]] {
				return m, errors.New("invalid package identity")
			}
			seen[parts[0]] = true
			switch parts[0] {
			case "format":
				if parts[1] != "oss-transfer-go-v1" {
					return m, errors.New("unknown package format")
				}
			case "version":
				m.version = parts[1]
			case "commit":
				m.commit = parts[1]
			}
		case "archive":
			if len(parts) != 6 || !packageNamePattern.MatchString(parts[3]) {
				return m, errors.New("invalid package archive")
			}
			ext := ".tar.gz"
			if parts[1] == "windows" {
				ext = ".zip"
			}
			if parts[3] != "oss-transfer-"+parts[1]+"-"+parts[2]+ext {
				return m, errors.New("package platform does not match archive")
			}
			name, checksum, length = parts[3], parts[4], parts[5]
		case "asset":
			if len(parts) != 4 {
				return m, errors.New("invalid package asset")
			}
			if parts[1] != "install.sh" && parts[1] != "native-install.sh" && parts[1] != "Install.ps1" {
				return m, errors.New("unexpected package asset")
			}
			name, checksum, length = parts[1], parts[2], parts[3]
		case "file":
			// Binary records are consumed by the installers after extraction.
			// They are never downloadable as individual files from this route.
			if len(parts) != 7 || !packageHashPattern.MatchString(parts[5]) {
				return m, errors.New("invalid binary record")
			}
			key := strings.Join(parts[1:4], "/")
			if m.records[key] != "" {
				return m, errors.New("duplicate binary record")
			}
			m.records[key] = line
			continue
		default:
			return m, errors.New("unknown package manifest record")
		}
		if name == "" {
			continue
		}
		size, err := strconv.ParseInt(length, 10, 64)
		if err != nil || size < 1 || size > agentPackageLimit || !packageHashPattern.MatchString(checksum) || m.files[name].name != "" {
			return m, errors.New("invalid package size or checksum")
		}
		if strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".ps1") {
			if size > 1<<20 {
				return m, errors.New("installer exceeds size limit")
			}
		}
		m.files[name] = agentPackage{name: name, checksum: checksum, size: size}
	}
	if !seen["format"] || !artifactVersion.MatchString(m.version) || !packageCommitPattern.MatchString(m.commit) || len(m.files) == 0 {
		return m, errors.New("incomplete package manifest")
	}
	return m, nil
}

// Release binaries carry a short commit while manifests carry the full ID.
// A dirty development binary must never silently serve a clean release build.
func matchingPackageBuild(running, packaged string) bool {
	return runningPackageCommitPattern.MatchString(running) && packageCommitPattern.MatchString(packaged) &&
		strings.HasSuffix(running, "-dirty") == strings.HasSuffix(packaged, "-dirty") &&
		strings.HasPrefix(strings.TrimSuffix(packaged, "-dirty"), strings.TrimSuffix(running, "-dirty"))
}

func (s *Server) openAgentPackages() (*os.Root, agentPackageManifest, error) {
	var m agentPackageManifest
	if s.options.DataDirectory == "" {
		return nil, m, os.ErrNotExist
	}
	directory := filepath.Join(s.options.DataDirectory, "packages")
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, m, err
	}
	if !info.IsDir() {
		return nil, m, errors.New("invalid package directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, m, err
	}
	file, err := openPackageFile(root, "manifest.tsv")
	if err != nil {
		root.Close()
		return nil, m, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65536))
	file.Close()
	if err == nil {
		m, err = parseAgentPackageManifest(raw)
	}
	if err == nil && s.options.AgentArtifactVersion != "" && strings.TrimPrefix(m.version, "v") != strings.TrimPrefix(s.options.AgentArtifactVersion, "v") {
		err = errors.New("package version differs from Master release")
	}
	if err == nil && s.options.Updater != nil {
		commit := s.options.Updater.Info().Commit
		if runningPackageCommitPattern.MatchString(commit) && !matchingPackageBuild(commit, m.commit) {
			err = errors.New("package commit differs from running Master")
		}
	}
	if err != nil {
		root.Close()
		return nil, m, err
	}
	return root, m, nil
}
func openPackageFile(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("package must be a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("invalid package file")
	}
	return file, nil
}
func checkAgentPackage(file *os.File, asset agentPackage) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != asset.size {
		return errors.New("package size mismatch")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, asset.size+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.checksum {
		return errors.New("package checksum mismatch")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}
func (s *Server) cachedAgentPackages() bool {
	root, _, err := s.openAgentPackages()
	if err != nil {
		return false
	}
	root.Close()
	return true
}

func (s *Server) agentPackageDownload(w http.ResponseWriter, r *http.Request) {
	if _, err := s.agents.registry.Invitation(r.PathValue("token")); err != nil {
		fail(w, http.StatusNotFound, "Invalid or expired Agent invitation")
		return
	}
	name := r.PathValue("name")
	if name != "manifest.tsv" && name != "install.sh" && name != "native-install.sh" && name != "Install.ps1" && !packageNamePattern.MatchString(name) {
		fail(w, http.StatusNotFound, "Agent installation file not found")
		return
	}
	root, manifest, err := s.openAgentPackages()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "Master Agent packages are unavailable")
		return
	}
	defer root.Close()
	if name == "manifest.tsv" {
		if target := r.URL.Query().Get("target"); target != "" {
			archive := "oss-transfer-" + target + ".tar.gz"
			if strings.HasPrefix(target, "windows-") {
				archive = "oss-transfer-" + target + ".zip"
			}
			if !packageNamePattern.MatchString(archive) {
				fail(w, http.StatusBadRequest, "Unsupported Agent platform")
				return
			}
			if !manifest.hasPlatform(target) {
				manifest, err = s.completeAgentManifest(r.Context(), root, manifest, target)
				if err != nil {
					fail(w, http.StatusServiceUnavailable, "Requested Agent platform is not cached on Master and its matching release could not be downloaded")
					return
				}
			}
		}
		if _, err = s.agents.registry.Invitation(r.PathValue("token")); err != nil {
			fail(w, http.StatusNotFound, "Invalid or expired Agent invitation")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(manifest.raw)
		return
	}
	asset, ok := manifest.files[name]
	if !ok {
		fail(w, http.StatusNotFound, "Requested platform is not included in the Master package manifest")
		return
	}
	file, err := s.localAgentPackage(r.Context(), root, manifest, asset)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "Cannot load verified Agent package from Master")
		return
	}
	defer file.Close()
	// A download that waited behind another request must still have a live invite.
	if _, err = s.agents.registry.Invitation(r.PathValue("token")); err != nil {
		fail(w, http.StatusNotFound, "Invalid or expired Agent invitation")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, time.Time{}, file)
}

func verifiedLocalAgentPackage(root *os.Root, asset agentPackage) (*os.File, error) {
	file, err := openPackageFile(root, asset.name)
	if err != nil {
		return nil, err
	}
	if err = checkAgentPackage(file, asset); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (s *Server) localAgentPackage(ctx context.Context, root *os.Root, manifest agentPackageManifest, asset agentPackage) (*os.File, error) {
	// Existing packages never wait for a slow upstream platform download.
	file, err := verifiedLocalAgentPackage(root, asset)
	if err == nil {
		return file, nil
	}
	// Corruption must not be hidden by silently fetching another build.
	if !errors.Is(err, os.ErrNotExist) || s.agentReleaseURL() == "" {
		return nil, err
	}
	// Bound concurrent upstream downloads and coalesce requests for the same file.
	select {
	case s.agents.packageSlot <- struct{}{}:
		defer func() { <-s.agents.packageSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// The previous download holder may have populated this file already.
	file, err = verifiedLocalAgentPackage(root, asset)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	address := s.agentReleaseURL() + "/" + asset.name
	if s.agentArtifactBase() == "https://github.com/MoonWX/oss-transfer/releases/download" && !packageNamePattern.MatchString(asset.name) {
		path := asset.name
		if path == "native-install.sh" {
			path = "deploy/native/install.sh"
		}
		if path == "Install.ps1" {
			path = "deploy/native/Install.ps1"
		}
		address = "https://raw.githubusercontent.com/MoonWX/oss-transfer/" + manifest.commit + "/" + path
	}
	client := &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.User != nil || !safePackageOrigin(req.URL) {
			return errors.New("unsafe package redirect")
		}
		return nil
	}}
	u, err := url.Parse(address)
	if err != nil || !safePackageOrigin(u) {
		return nil, errors.New("invalid package origin")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > asset.size {
		return nil, fmt.Errorf("package server returned HTTP %d or unexpected size", response.StatusCode)
	}
	token, err := cluster.Token()
	if err != nil {
		return nil, err
	}
	temp := ".download-" + token
	file, err = root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer root.Remove(temp)
	_, err = io.Copy(file, io.LimitReader(response.Body, asset.size+1))
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = checkAgentPackage(file, asset)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	// Windows cannot replace an open file; close the staging handle before
	// publishing the verified download, then open the published file for serving.
	if err = file.Close(); err != nil {
		return nil, err
	}
	if err = root.Rename(temp, asset.name); err != nil {
		return nil, err
	}
	file, err = openPackageFile(root, asset.name)
	if err != nil {
		return nil, err
	}
	if err = checkAgentPackage(file, asset); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (m agentPackageManifest) hasPlatform(target string) bool {
	parts := strings.SplitN(target, "-", 2)
	if len(parts) != 2 {
		return false
	}
	name := "oss-transfer-" + target + ".tar.gz"
	if parts[0] == "windows" {
		name = "oss-transfer-" + target + ".zip"
	}
	if m.files[name].name == "" {
		return false
	}
	for _, role := range []string{"master", "agent", "cli"} {
		if m.records[parts[0]+"/"+parts[1]+"/"+role] == "" {
			return false
		}
	}
	return true
}

// Only a missing platform requires network access. Reusing a cached platform
// works in an isolated network, including the installer's manifest request.
func (s *Server) completeAgentManifest(ctx context.Context, root *os.Root, current agentPackageManifest, target string) (agentPackageManifest, error) {
	select {
	case s.agents.packageSlot <- struct{}{}:
		defer func() { <-s.agents.packageSlot }()
	case <-ctx.Done():
		return current, ctx.Err()
	}
	// A concurrent join may already have completed the same manifest.
	if file, err := openPackageFile(root, "manifest.tsv"); err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(file, 65536))
		file.Close()
		if newer, parseErr := parseAgentPackageManifest(raw); readErr == nil && parseErr == nil && newer.commit == current.commit && newer.version == current.version && newer.hasPlatform(target) {
			return newer, nil
		}
	}
	base := s.agentReleaseURL()
	if base == "" {
		return current, errors.New("no upstream package origin")
	}
	if s.agentArtifactBase() == "https://github.com/MoonWX/oss-transfer/releases/download" {
		base = officialAgentArtifactBase + "/v" + strings.TrimPrefix(current.version, "v")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/manifest.tsv", nil)
	if err != nil || !safePackageOrigin(request.URL) {
		return current, errors.New("invalid package manifest origin")
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !safePackageOrigin(req.URL) {
			return errors.New("unsafe package redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return current, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return current, errors.New("package manifest unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	if err != nil {
		return current, err
	}
	complete, err := parseAgentPackageManifest(raw)
	if err != nil {
		return current, err
	}
	if complete.commit != current.commit || strings.TrimPrefix(complete.version, "v") != strings.TrimPrefix(current.version, "v") {
		return current, errors.New("package build differs from Master")
	}
	for name, asset := range current.files {
		if complete.files[name] != asset {
			return current, errors.New("package manifest changes a verified asset")
		}
	}
	for name, record := range current.records {
		if complete.records[name] != record {
			return current, errors.New("package manifest changes a verified binary")
		}
	}
	if !complete.hasPlatform(target) {
		return current, errors.New("upstream does not include requested platform")
	}
	token, err := cluster.Token()
	if err != nil {
		return current, err
	}
	temp := ".manifest-" + token
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return current, err
	}
	defer root.Remove(temp)
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err == nil {
		err = root.Rename(temp, "manifest.tsv")
	}
	if err != nil {
		return current, err
	}
	return complete, nil
}

func safePackageOrigin(u *url.URL) bool {
	if u == nil || u.User != nil || u.Hostname() == "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}
