// Package softwareupdate stages verified releases and delegates replacement to
// the native installer in a process independent of the service being upgraded.
package softwareupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

type Options struct {
	Role, Version, Commit, DataDirectory, ConfigPath, ServiceName string
}

type Info struct {
	Role      string `json:"role"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Platform  string `json:"platform"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type Status struct {
	ID        string    `json:"id,omitempty"`
	Version   string    `json:"version,omitempty"`
	Phase     string    `json:"phase"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	Log       string    `json:"log,omitempty"`
}

type installation struct {
	Role        string `json:"role"`
	ServiceName string `json:"serviceName"`
	Config      string `json:"config"`
	DataDir     string `json:"dataDir"`
	CLIPath     string `json:"cliPath"`
	Unit        string `json:"unit"`
	Version     string `json:"version"`
}

type request struct {
	ID         string  `json:"id"`
	Version    string  `json:"version"`
	Options    Options `json:"options"`
	Executable string  `json:"executable"`
}

type Manager struct {
	mu         sync.Mutex
	options    Options
	executable string
	client     *client
	launch     func(string, string, string) error
	info       Info
	lastCheck  time.Time
	release    Release
}

func New(options Options) *Manager {
	executable, _ := os.Executable()
	m := &Manager{options: options, executable: executable, client: newClient(), launch: launchHelper}
	m.info = Info{Role: options.Role, Version: options.Version, Commit: options.Commit, Platform: platform()}
	err := checkContainer()
	var installed installation
	if err == nil {
		installed, err = inspectInstallation(options, executable)
	}
	if err == nil {
		err = hostSupport()
	}
	if err == nil {
		err = checkInstallationPrivileges(installed)
	}
	if err != nil {
		m.info.Reason, m.info.Detail = "UNMANAGED", err.Error()
		var unsupported *capabilityError
		if errors.As(err, &unsupported) {
			m.info.Reason = unsupported.code
		}
	} else {
		m.info.Supported = true
	}
	return m
}

func (m *Manager) Info() Info { return m.info }

func (m *Manager) Check(ctx context.Context) (Release, error) {
	m.mu.Lock()
	if !m.lastCheck.IsZero() && time.Since(m.lastCheck) < time.Minute {
		release := m.release
		m.mu.Unlock()
		return release, nil
	}
	m.mu.Unlock()
	release, err := m.client.release(ctx, "latest")
	if err != nil {
		return Release{}, err
	}
	m.mu.Lock()
	m.release, m.lastCheck = release, time.Now()
	m.mu.Unlock()
	return release, nil
}

func (m *Manager) statusPath() string {
	return filepath.Join(m.options.DataDirectory, "runtime", "updates", "status.json")
}

func (m *Manager) Status() Status {
	if !filepath.IsAbs(m.options.DataDirectory) {
		return Status{Phase: "IDLE"}
	}
	status := Status{Phase: "IDLE"}
	if err := readJSON(m.statusPath(), &status); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Status{Phase: "FAILED", Error: "Cannot read update status: " + err.Error()}
		}
		return Status{Phase: "IDLE"}
	}
	if active(status.Phase) && time.Since(status.UpdatedAt) > time.Hour {
		status.Phase, status.Error = "FAILED", "The update helper did not finish; inspect the service before retrying."
	}
	return status
}

func active(phase string) bool {
	return phase == "QUEUED" || phase == "DOWNLOADING" || phase == "APPLYING"
}

// StartsWork identifies commands which create or resume data-plane work. Read
// and stop commands remain available while a native upgrade is in progress.
func StartsWork(operation string) bool {
	switch operation {
	case "child_accept", "child_recover", "scan_start", "scan_resume", "parent_resume", "comparison_start", "inventory_begin":
		return true
	}
	return false
}

func InProgress(phase string) bool { return active(phase) }

func (m *Manager) Start(ctx context.Context, version string) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	if !m.info.Supported {
		message := m.info.Detail
		if message == "" {
			message = m.info.Reason
		}
		return Status{}, errors.New(message)
	}
	version = strings.TrimPrefix(version, "v")
	if !stableVersion.MatchString(version) {
		return Status{}, errors.New("Only a newer stable release can be installed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.Status()
	if active(previous.Phase) {
		if previous.Version == version {
			return previous, nil
		}
		return previous, errors.New("Another update is already in progress")
	}
	if compareVersions(version, m.options.Version) <= 0 {
		return Status{}, errors.New("Only a newer stable release can be installed")
	}
	if _, err := inspectInstallation(m.options, m.executable); err != nil {
		return Status{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Status{}, err
	}
	id := hex.EncodeToString(nonce[:])
	work := filepath.Join(filepath.Dir(m.statusPath()), id)
	if err := privateDirectory(work); err != nil {
		return Status{}, err
	}
	status := Status{ID: id, Version: version, Phase: "QUEUED", UpdatedAt: time.Now().UTC()}
	if err := writeJSON(m.statusPath(), status); err != nil {
		return Status{}, err
	}
	helper := filepath.Join(work, "update-helper"+executableSuffix())
	requestPath := filepath.Join(work, "request.json")
	err := copyRegular(m.executable, helper, 0755, 512<<20)
	if err == nil {
		err = writeJSON(requestPath, request{ID: id, Version: version, Options: m.options, Executable: m.executable})
	}
	if err == nil {
		err = m.launch(helper, requestPath, filepath.Join(work, "helper.log"))
	}
	if err != nil {
		status.Phase, status.Error, status.UpdatedAt = "FAILED", err.Error(), time.Now().UTC()
		_ = writeJSON(m.statusPath(), status)
		return status, err
	}
	return status, nil
}

func inspectInstallation(options Options, executable string) (installation, error) {
	var installed installation
	if options.Role != "master" && options.Role != "agent" {
		return installed, errors.New("Unsupported service role")
	}
	if !stableVersion.MatchString(options.Version) {
		return installed, errors.New("Online updates require a stable installed release")
	}
	for _, path := range []string{options.DataDirectory, options.ConfigPath, executable} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n\t") {
			return installed, errors.New("Online updates require a managed native installation")
		}
	}
	if filepath.Base(executable) != "oss-transfer-"+options.Role+executableSuffix() {
		return installed, errors.New("The running binary does not match the managed installation")
	}
	if err := regular(executable); err != nil {
		return installed, err
	}
	metadata := filepath.Join(filepath.Dir(executable), "install.tsv")
	if runtime.GOOS == "windows" {
		metadata = filepath.Join(filepath.Dir(executable), "install.json")
		// Existing Windows metadata includes backupDir; its unknown fields are
		// harmless, but identities below are always validated against the host.
		raw, err := readLimited(metadata, 65536)
		if err != nil {
			return installed, errors.New("No managed installation metadata found")
		}
		if err = json.Unmarshal(raw, &installed); err != nil {
			return installed, err
		}
	} else {
		raw, err := readLimited(metadata, 65536)
		if err != nil {
			return installed, errors.New("No managed installation metadata found")
		}
		fields := map[string]string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			key, value, ok := strings.Cut(line, "\t")
			if _, duplicate := fields[key]; !ok || duplicate || strings.ContainsAny(value, "\r\n\t") {
				return installed, errors.New("Invalid installation metadata")
			}
			fields[key] = value
		}
		installed = installation{Role: fields["role"], ServiceName: fields["serviceName"], Config: fields["config"], DataDir: fields["dataDir"], CLIPath: fields["cliPath"], Unit: fields["unit"], Version: fields["version"]}
	}
	if err := regular(options.ConfigPath); err != nil {
		return installed, err
	}
	config, err := servicehost.LoadConfig(options.ConfigPath, options.Role)
	if err != nil {
		return installed, err
	}
	if installed.Role != options.Role || installed.Version != options.Version || installed.ServiceName != options.ServiceName || installed.Config != options.ConfigPath || config.ServiceName != options.ServiceName || config.DataDirectory != options.DataDirectory || (installed.DataDir != "" && installed.DataDir != options.DataDirectory) {
		return installed, errors.New("The service identity differs from its installation metadata")
	}
	if !filepath.IsAbs(installed.CLIPath) || filepath.Clean(installed.CLIPath) != installed.CLIPath || strings.ContainsAny(installed.CLIPath, "\x00\r\n\t") {
		return installed, errors.New("Managed CLI path is missing or invalid")
	}
	if err = regular(installed.CLIPath); err != nil {
		return installed, err
	}
	return installed, nil
}

func checkHost() error {
	if err := checkContainer(); err != nil {
		return err
	}
	return hostSupport()
}
func checkContainer() error {
	for _, path := range []string{"/.dockerenv", "/usr/local/share/oss-transfer/provenance.json"} {
		if _, err := os.Stat(path); err == nil {
			return &capabilityError{"CONTAINER", "Container installations must be updated with Docker Compose"}
		}
	}
	return nil
}

type capabilityError struct{ code, message string }

func (e *capabilityError) Error() string { return e.message }

func platform() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return runtime.GOOS + "/" + arch
}
func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func compareVersions(a, b string) int {
	if !stableVersion.MatchString(a) || !stableVersion.MatchString(b) {
		return 0
	}
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	for i := range x {
		left, _ := strconv.ParseUint(x[i], 10, 32)
		right, _ := strconv.ParseUint(y[i], 10, 32)
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
	}
	return 0
}

func regular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Expected a regular file: %s", path)
	}
	return nil
}

func privateDirectory(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && !info.IsDir() {
			if runtime.GOOS == "darwin" && (current == "/tmp" || current == "/var" || current == "/etc") {
				continue
			}
			return errors.New("Update directory contains a symlink or non-directory")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return protectDirectory(path)
}

func readLimited(path string, limit int64) ([]byte, error) {
	if err := regular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = errors.New("Update metadata is too large")
	}
	return raw, err
}
func readJSON(path string, target any) error {
	raw, err := readLimited(path, 65536)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
func writeJSON(path string, value any) error {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := regular(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}
func copyRegular(source, destination string, mode os.FileMode, limit int64) error {
	if err := regular(source); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if n > limit {
		err = errors.New("Update executable is too large")
	}
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}
