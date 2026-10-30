package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type localCLIAuth struct {
	MasterURL string `json:"masterUrl"`
	Role      string `json:"role"`
	Token     string `json:"token"`
}

type localAuthLocation struct{ connection, credential string }

// The public origin is checked before opening its private credential. A local
// installation never lends its administrative access to another Master.
func loadLocalCLIAuth(host string) (string, error) {
	origin, err := masterOrigin(host, "host")
	if err != nil {
		return "", err
	}
	locations := localAuthLocations()
	seen := map[string]bool{}
	for _, location := range locations {
		if seen[location.credential] {
			continue
		}
		seen[location.credential] = true
		installed, found, err := readCLIConnectionFile(location.connection)
		if err != nil || !found || installed != origin {
			continue
		}
		token, err := readLocalCLIAuth(location.credential, origin)
		if err != nil || token != "" {
			return token, err
		}
	}
	return "", nil
}

func localAuthLocations() []localAuthLocation {
	var result []localAuthLocation
	addCLI := func(path string) {
		if filepath.IsAbs(path) {
			result = append(result, localAuthLocation{path + ".cluster.json", path + ".local-auth.json"})
		}
	}
	addData := func(path string) {
		if filepath.IsAbs(path) {
			result = append(result, localAuthLocation{filepath.Join(path, "cli-cluster.json"), filepath.Join(path, "cli-local-auth.json")})
		}
	}
	executable, _ := cliExecutable()
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	addCLI(executable)
	for _, path := range cliGlobalPaths() {
		addCLI(path)
	}
	for _, path := range cliContainerPaths() {
		addData(filepath.Dir(path))
	}
	directories := append([]string{filepath.Dir(executable)}, cliInstallationDirs()...)
	for _, directory := range directories {
		name := "install.tsv"
		if runtime.GOOS == "windows" {
			name = "install.json"
		}
		raw, found, err := readDiscoveryFile(filepath.Join(directory, name))
		if err != nil || !found {
			continue
		}
		fields := map[string]string{}
		if name == "install.json" {
			if json.Unmarshal(raw, &fields) != nil {
				continue
			}
		} else {
			valid := true
			for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
				key, value, ok := strings.Cut(line, "\t")
				if !ok || fields[key] != "" {
					valid = false
					break
				}
				fields[key] = value
			}
			if !valid {
				continue
			}
		}
		if fields["role"] != "master" && fields["role"] != "agent" {
			continue
		}
		addCLI(fields["cliPath"])
		addData(fields["dataDir"])
	}
	return result
}

func readLocalCLIAuth(path, origin string) (string, error) {
	if directory, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(directory, filepath.Base(path))
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return "", cliErrorf("本机 CLI 凭据文件无效: %s", path)
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrPermission) {
		// Ordinary OS users can still use an account session.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return "", cliErrorf("本机 CLI 凭据文件无效: %s", path)
	}
	if err := trustedDiscoveryFile(path, current); err != nil {
		return "", err
	}
	if err := privateLocalAuthFile(path, current); err != nil {
		return "", cliErrorf("本机 CLI 凭据文件权限不安全: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return "", err
	}
	var value localCLIAuth
	if len(data) > 65536 || json.Unmarshal(data, &value) != nil || (value.Role != "master" && value.Role != "agent") || len(value.Token) < 32 || len(value.Token) > 4096 || strings.ContainsAny(value.Token, " \r\n\t") {
		return "", cliErrorf("本机 CLI 凭据文件无效: %s", path)
	}
	bound, err := masterOrigin(value.MasterURL, "host")
	if err != nil || bound != origin {
		return "", cliErrorf("本机 CLI 凭据与集群地址不匹配: %s", path)
	}
	return value.Token, nil
}
