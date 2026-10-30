package servicehost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// LocalMasterOrigin turns a listening address into an address usable on this host.
func LocalMasterOrigin(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// PublishCLIConnection publishes only a service origin, never its Agent identity
// or credentials. Managed installations refresh it on every service start, so
// an upgrade also adopts installations created before CLI discovery existed.
func PublishCLIConnection(role, origin string, config Config) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return publishCLIConnection(executable, goruntime.GOOS, role, origin, config)
}

func publishCLIConnection(executable, platform, role, origin string, config Config) error {
	paths, err := cliPublicationPaths(executable, platform, role, origin, config, "cli-cluster.json", ".cluster.json")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := writeCLIConnection(path, role, origin); err != nil {
			return err
		}
	}
	return nil
}

// PublishCLILocalAuth makes the installation's administrative credential
// available only to its OS owner and system administrators. Public discovery
// files remain usable by other users and never contain this credential.
func PublishCLILocalAuth(role, origin, token string, config Config) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return publishCLILocalAuth(executable, goruntime.GOOS, role, origin, token, config)
}

func publishCLILocalAuth(executable, platform, role, origin, token string, config Config) error {
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("invalid local CLI credential")
	}
	paths, err := cliPublicationPaths(executable, platform, role, origin, config, "cli-local-auth.json", ".local-auth.json")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := writeCLILocalAuth(path, role, origin, token); err != nil {
			return err
		}
	}
	return nil
}

func cliPublicationPaths(executable, platform, role, origin string, config Config, dataName, suffix string) ([]string, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("invalid CLI Master origin")
	}
	if role != "master" && role != "agent" {
		return nil, errors.New("invalid CLI service role")
	}
	if !absolute(config.DataDirectory) {
		return nil, errors.New("invalid service data directory")
	}
	// Read-only container images keep their public CLI connection in the data
	// volume. Native services also publish a copy beside their installed CLI.
	paths := []string{filepath.Join(config.DataDirectory, dataName)}
	metadata := filepath.Join(filepath.Dir(executable), "install.tsv")
	if platform == "windows" {
		metadata = filepath.Join(filepath.Dir(executable), "install.json")
	}
	info, err := os.Lstat(metadata)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, errors.New("invalid installation metadata")
	}
	raw, err := os.ReadFile(metadata)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	if platform == "windows" {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
	} else {
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			key, value, ok := strings.Cut(line, "\t")
			if !ok || fields[key] != "" {
				return nil, errors.New("invalid installation metadata")
			}
			fields[key] = value
		}
	}
	if fields["role"] != role || fields["serviceName"] != config.ServiceName || (fields["dataDir"] != "" && fields["dataDir"] != config.DataDirectory) {
		return nil, errors.New("CLI installation does not match the running service")
	}
	cli := fields["cliPath"]
	if !absolute(cli) {
		return nil, errors.New("invalid installed CLI path")
	}
	if info, err := os.Lstat(cli); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("installed CLI is not a regular file")
	}
	return append(paths, cli+suffix), nil
}

func writeCLILocalAuth(path, role, origin, token string) error {
	if err := regularOrAbsent(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cli-local-auth-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	// Set Windows DACLs before writing any secret, not just on the final name.
	if err := privateFile(file); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(struct {
		MasterURL string `json:"masterUrl"`
		Role      string `json:"role"`
		Token     string `json:"token"`
	}{origin, role, token}); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish local CLI access: %w", err)
	}
	return nil
}

func writeCLIConnection(path, role, origin string) error {
	if err := regularOrAbsent(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cli-cluster-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(struct {
		MasterURL string `json:"masterUrl"`
		Role      string `json:"role"`
	}{origin, role}); err != nil {
		return err
	}
	if err := file.Chmod(0644); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish CLI connection: %w", err)
	}
	return nil
}
