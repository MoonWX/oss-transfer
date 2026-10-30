package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var cliExecutable = os.Executable
var cliInstallationDirs = nativeCLIInstallationDirs
var cliGlobalPaths = nativeCLIGlobalPaths
var cliContainerPaths = containerCLIConnections

func containerCLIConnections() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return []string{"/data/master/cli-cluster.json", "/data/cli-cluster.json"}
		}
	}
	return nil
}

func nativeCLIInstallationDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{filepath.Join(os.Getenv("ProgramFiles"), "OSS Transfer", "master"), filepath.Join(os.Getenv("ProgramFiles"), "OSS Transfer", "agent")}
	case "darwin":
		return []string{filepath.Join(home, "Library/Application Support/OSS Transfer/master/bin"), filepath.Join(home, "Library/Application Support/OSS Transfer/agent/bin"), "/usr/local/lib/oss-transfer/master", "/usr/local/lib/oss-transfer/agent"}
	case "freebsd":
		return []string{"/usr/local/lib/oss-transfer/master", "/usr/local/lib/oss-transfer/agent"}
	default:
		return []string{"/opt/oss-transfer/master", "/opt/oss-transfer/agent", "/opt/oss-transfer-go/master", "/opt/oss-transfer-go/agent"}
	}
}

func nativeCLIGlobalPaths() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".local/bin/oss-transfer-cli"), "/usr/local/bin/oss-transfer-cli", "/usr/bin/oss-transfer-cli"}
}

func readDiscoveryFile(path string) ([]byte, bool, error) {
	// Resolve parent aliases such as macOS /var -> /private/var, but reject a
	// link in place of the connection file itself below.
	if directory, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(directory, filepath.Base(path))
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, true, cliErrorf("集群连接文件无效: %s", path)
	}
	if err := trustedDiscoveryFile(path, info); err != nil {
		return nil, true, err
	}
	raw, err := os.ReadFile(path)
	return raw, true, err
}

func readCLIConnection(cli string) (string, bool, error) {
	return readCLIConnectionFile(cli + ".cluster.json")
}

func readCLIConnectionFile(path string) (string, bool, error) {
	raw, found, err := readDiscoveryFile(path)
	if err != nil || !found {
		return "", found, err
	}
	var value struct {
		MasterURL string `json:"masterUrl"`
		Role      string `json:"role"`
	}
	if json.Unmarshal(raw, &value) != nil || (value.Role != "master" && value.Role != "agent") {
		return "", true, cliErrorf("集群连接文件无效: %s", path)
	}
	origin, err := masterOrigin(value.MasterURL, "host")
	return origin, true, err
}

// Old native Masters can be discovered before their first upgrade. Agent
// origins are deliberately never read from a running or stopped Agent database.
func installationMaster(directory, matchingCLI string) (string, bool, error) {
	name := "install.tsv"
	if runtime.GOOS == "windows" {
		name = "install.json"
	}
	raw, found, err := readDiscoveryFile(filepath.Join(directory, name))
	if err != nil || !found {
		return "", found, err
	}
	fields := map[string]string{}
	if name == "install.json" {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return "", true, cliErrorf("安装信息无效: %s", directory)
		}
	} else {
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			key, value, ok := strings.Cut(line, "\t")
			if !ok || fields[key] != "" {
				return "", true, cliErrorf("安装信息无效: %s", directory)
			}
			fields[key] = value
		}
	}
	if matchingCLI != "" {
		installedCLI := fields["cliPath"]
		if resolved, err := filepath.EvalSymlinks(installedCLI); err == nil {
			installedCLI = resolved
		}
		if installedCLI != matchingCLI {
			return "", false, nil
		}
	}
	if cli := fields["cliPath"]; filepath.IsAbs(cli) {
		if origin, found, err := readCLIConnection(cli); found || err != nil {
			return origin, found, err
		}
	}
	if fields["role"] == "agent" {
		return "", true, cliErrorf("Agent 尚未生成集群连接信息，请重启升级后的 Agent，或使用 --host 指定 Master")
	}
	if fields["role"] != "master" || !filepath.IsAbs(fields["config"]) {
		return "", true, cliErrorf("安装信息无效: %s", directory)
	}
	raw, found, err = readDiscoveryFile(fields["config"])
	if err != nil || !found {
		return "", true, cliErrorf("无法读取 Master 配置，请使用 --host 指定地址")
	}
	var config struct {
		Listen      string `json:"listen"`
		ServiceName string `json:"serviceName"`
	}
	if json.Unmarshal(raw, &config) != nil || config.ServiceName != fields["serviceName"] {
		return "", true, cliErrorf("安装信息无效: %s", directory)
	}
	host, port, err := net.SplitHostPort(config.Listen)
	if err != nil {
		return "", true, cliErrorf("Master 监听地址无效")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	origin, err := masterOrigin("http://"+net.JoinHostPort(host, port), "host")
	return origin, true, err
}

func discoveredMaster() (string, error) {
	if preference, err := loadCLIPreferences(); err == nil && preference.Master != "" {
		return masterOrigin(preference.Master, "host")
	} else if err != nil {
		return "", err
	}
	executable, err := cliExecutable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	if origin, found, err := readCLIConnection(executable); found || err != nil {
		return origin, err
	}
	if origin, found, err := installationMaster(filepath.Dir(executable), executable); found || err != nil {
		return origin, err
	}
	selectUnique := func(origins map[string]bool) (string, error) {
		if len(origins) > 1 {
			return "", cliErrorf("检测到多个集群，请使用 --host 选择，或通过 config set master 保存默认 Master")
		}
		for origin := range origins {
			return origin, nil
		}
		return "", nil
	}
	origins := map[string]bool{}
	for _, cli := range cliGlobalPaths() {
		origin, found, err := readCLIConnection(cli)
		if err != nil {
			return "", fmt.Errorf("%s: %w", cli, err)
		}
		if found {
			origins[origin] = true
		}
	}
	if origin, err := selectUnique(origins); origin != "" || err != nil {
		return origin, err
	}
	for _, path := range cliContainerPaths() {
		origin, found, err := readCLIConnectionFile(path)
		if err != nil {
			return "", err
		}
		if found {
			origins[origin] = true
		}
	}
	if origin, err := selectUnique(origins); origin != "" || err != nil {
		return origin, err
	}
	var unresolved error
	for _, directory := range cliInstallationDirs() {
		origin, found, err := installationMaster(directory, "")
		if err != nil {
			unresolved = err
			continue
		}
		if found {
			origins[origin] = true
		}
	}
	// Do not guess another cluster when a detected installation is unreadable.
	if unresolved != nil {
		return "", unresolved
	}
	if origin, err := selectUnique(origins); origin != "" || err != nil {
		return origin, err
	}
	return "http://localhost:1685", nil
}
