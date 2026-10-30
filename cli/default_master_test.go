package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func discoveryTestPaths(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	oldExecutable, oldDirs, oldGlobals := cliExecutable, cliInstallationDirs, cliGlobalPaths
	oldContainer := cliContainerPaths
	t.Cleanup(func() {
		cliExecutable, cliInstallationDirs, cliGlobalPaths = oldExecutable, oldDirs, oldGlobals
		cliContainerPaths = oldContainer
	})
	dir := t.TempDir()
	executable := filepath.Join(dir, "oss-transfer-cli")
	if err := os.WriteFile(executable, []byte("cli"), 0755); err != nil {
		t.Fatal(err)
	}
	cliExecutable = func() (string, error) { return executable, nil }
	cliInstallationDirs = func() []string { return nil }
	cliGlobalPaths = func() []string { return nil }
	cliContainerPaths = func() []string { return nil }
	return dir, executable
}

func writeTestConnection(t *testing.T, cli, role, origin string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"role": role, "masterUrl": origin})
	if err := os.WriteFile(cli+".cluster.json", raw, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultMasterDiscoveryAndExplicitOverrides(t *testing.T) {
	_, binary := discoveryTestPaths(t)
	writeTestConnection(t, binary, "master", "http://127.0.0.1:1688")
	for _, role := range []string{"master", "agent"} {
		writeTestConnection(t, binary, role, "http://10.1.0.43:1688")
		got, err := parseGlobals([]string{"task", "list"})
		if err != nil || got.host != "http://10.1.0.43:1688" {
			t.Fatal(got, err)
		}
	}
	runLocalConfig(t, "config", "set", "master", "https://remote.example:8443")
	got, err := parseGlobals([]string{"task", "list"})
	if err != nil || got.host != "https://remote.example:8443" {
		t.Fatal(got, err)
	}
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", "http://environment.example:9000")
	got, err = parseGlobals([]string{"task", "list"})
	if err != nil || got.host != "http://environment.example:9000" {
		t.Fatal(got, err)
	}
	got, err = parseGlobals([]string{"task", "list", "--host", "https://override.example"})
	if err != nil || got.host != "https://override.example" {
		t.Fatal(got, err)
	}
	pref, err := loadCLIPreferences()
	if err != nil || pref.Master != "https://remote.example:8443" {
		t.Fatal("one-off override changed default", pref, err)
	}
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", "")
	runLocalConfig(t, "config", "set", "language", "ja")
	pref, _ = loadCLIPreferences()
	if pref.Master != "https://remote.example:8443" {
		t.Fatal("language change lost master")
	}
	runLocalConfig(t, "config", "set", "master", "auto")
	got, err = parseGlobals([]string{"task", "list"})
	if err != nil || got.host != "http://10.1.0.43:1688" {
		t.Fatal(got, err)
	}
}

func TestDefaultMasterRejectsAmbiguousOrUnsafeDiscovery(t *testing.T) {
	dir, binary := discoveryTestPaths(t)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeTestConnection(t, a, "master", "http://127.0.0.1:1688")
	writeTestConnection(t, b, "agent", "http://10.1.0.170:1685")
	cliGlobalPaths = func() []string { return []string{a, b} }
	if _, err := discoveredMaster(); err == nil {
		t.Fatal("ambiguous cluster selected")
	}
	if err := os.Symlink(a+".cluster.json", binary+".cluster.json"); err == nil {
		if _, err := discoveredMaster(); err == nil {
			t.Fatal("symlink connection accepted")
		}
		os.Remove(binary + ".cluster.json")
	}
	if runtime.GOOS != "windows" {
		writeTestConnection(t, binary, "agent", "http://unsafe.example")
		os.Chmod(binary+".cluster.json", 0666)
		if _, err := discoveredMaster(); err == nil {
			t.Fatal("writable connection accepted")
		}
	}
	// Explicit selection does not depend on a damaged or ambiguous local file.
	if got, err := parseGlobals([]string{"whoami", "--host", "https://chosen.example"}); err != nil || got.host != "https://chosen.example" {
		t.Fatal(got, err)
	}
}

func TestOldMasterInstallationDiscoversCustomPortWithoutReadingDatabase(t *testing.T) {
	dir, binary := discoveryTestPaths(t)
	config := filepath.Join(dir, "service.json")
	os.WriteFile(config, []byte(`{"listen":":1688","serviceName":"custom-master"}`), 0600)
	fields := map[string]string{"role": "master", "serviceName": "custom-master", "config": config, "cliPath": binary}
	name := "install.tsv"
	var raw []byte
	if runtime.GOOS == "windows" {
		name = "install.json"
		raw, _ = json.Marshal(fields)
	} else {
		for k, v := range fields {
			raw = append(raw, []byte(k+"\t"+v+"\n")...)
		}
	}
	os.WriteFile(filepath.Join(dir, name), raw, 0600)
	os.WriteFile(filepath.Join(dir, "agent.db"), []byte("must never read this database or its credentials"), 0000)
	if got, err := discoveredMaster(); err != nil || got != "http://127.0.0.1:1688" {
		t.Fatal(got, err)
	}
}

func TestDefaultMasterConfigRejectsCredentialURLWithoutChangingPreferences(t *testing.T) {
	discoveryTestPaths(t)
	runLocalConfig(t, "config", "set", "master", "https://cluster.example")
	var out, stderr bytes.Buffer
	if code := runCLI([]string{"config", "set", "master", "https://user:secret@bad.example"}, nil, &out, &stderr); code != 2 {
		t.Fatal(code, stderr.String())
	}
	if strings.Contains(stderr.String(), "secret") {
		t.Fatal("URL credentials echoed")
	}
	pref, _ := loadCLIPreferences()
	if pref.Master != "https://cluster.example" {
		t.Fatal("invalid value replaced default")
	}
}

func TestUpdateVersionDoesNotBecomeGlobalVersionOutput(t *testing.T) {
	discoveryTestPaths(t)
	for _, args := range [][]string{{"update", "master", "--version", "4.1.22", "--yes"}, {"update", "agent", "--version=4.1.22", "--worker", "one", "--yes"}} {
		got, err := parseGlobals(args)
		if err != nil || got.version || len(got.args) != len(args) {
			t.Fatal(got, err)
		}
	}
	if got, err := parseGlobals([]string{"--version"}); err != nil || !got.version {
		t.Fatal(got, err)
	}
}

func TestContainerAgentDiscoversDataVolumeConnection(t *testing.T) {
	dir, _ := discoveryTestPaths(t)
	path := filepath.Join(dir, "cli-cluster.json")
	if err := os.WriteFile(path, []byte(`{"role":"agent","masterUrl":"http://master:1685"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cliContainerPaths = func() []string { return []string{filepath.Join(dir, "master", "cli-cluster.json"), path} }
	if got, err := discoveredMaster(); err != nil || got != "http://master:1685" {
		t.Fatal(got, err)
	}
}

func TestMasterConfigGetShowsActualConnection(t *testing.T) {
	_, executable := discoveryTestPaths(t)
	writeTestConnection(t, executable, "agent", "http://10.1.0.43:1688")
	for _, tc := range []struct {
		args           []string
		master, source string
	}{
		{[]string{"--json", "config", "get", "master"}, "http://10.1.0.43:1688", "automatic"},
		{[]string{"--json", "--host", "https://another.example", "config", "get", "master"}, "https://another.example", "override"},
	} {
		var out, stderr bytes.Buffer
		if code := runCLI(tc.args, strings.NewReader(""), &out, &stderr); code != 0 {
			t.Fatal(code, stderr.String())
		}
		var result map[string]string
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result["master"] != tc.master || result["source"] != tc.source {
			t.Fatal(result)
		}
	}
}
