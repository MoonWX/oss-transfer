package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestVersionOutputShowsPrefixedVersionAndKeepsJSON(t *testing.T) {
	savedVersion, savedCommit := version, commit
	version, commit = "4.2.0", "33b320a"
	t.Cleanup(func() { version, commit = savedVersion, savedCommit })
	noNetwork := func(http.ResponseWriter, *http.Request) { t.Fatal("network during --version") }
	code, out, _ := runMockCLI(t, []string{"--version"}, noNetwork)
	if code != 0 || !strings.HasPrefix(out, "OSS Transfer CLI v4.2.0\n") || !strings.Contains(out, "33b320a") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = runMockCLI(t, []string{"--version", "--json"}, noNetwork)
	var machine map[string]string
	if err := json.Unmarshal([]byte(out), &machine); code != 0 || err != nil || machine["version"] != "4.2.0" || machine["commit"] != "33b320a" {
		t.Fatalf("machine output changed: %d %q %v", code, out, err)
	}
}

func TestSoftwareStatusShowsPrefixedVersions(t *testing.T) {
	var out bytes.Buffer
	renderSoftwareStatus(&out, map[string]interface{}{
		"agents": []interface{}{map[string]interface{}{"address": "10.0.0.2", "info": map[string]interface{}{"version": "4.1.41"}}},
		"latest": map[string]interface{}{"version": "4.2.0"},
	}, map[string]interface{}{"info": map[string]interface{}{"version": "4.2.0"}})
	text := out.String()
	if !strings.Contains(text, "v4.2.0") || !strings.Contains(text, "v4.1.41") || strings.Contains(text, " 4.2.0") {
		t.Fatalf("versions not shown as v<version>: %q", text)
	}
}

// A build without a branch (a tag or detached checkout) omits it.
func TestVersionOutputOmitsAnEmptyBranch(t *testing.T) {
	savedVersion, savedCommit, savedBranch := version, commit, branch
	version, commit, branch = "4.2.0", "33b320a", ""
	t.Cleanup(func() { version, commit, branch = savedVersion, savedCommit, savedBranch })
	code, out, _ := runMockCLI(t, []string{"--version"}, func(http.ResponseWriter, *http.Request) { t.Fatal("network during --version") })
	if code != 0 || !strings.Contains(out, "33b320a") || strings.Contains(out, "分支") || strings.Contains(out, "  ") {
		t.Fatalf("%d %q", code, out)
	}
}
