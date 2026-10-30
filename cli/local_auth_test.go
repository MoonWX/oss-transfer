package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testLocalToken = "local-cli.test-credential-never-print-1234567890"

func writeTestLocalAuth(t *testing.T, path, role, origin string) {
	t.Helper()
	raw, _ := json.Marshal(localCLIAuth{MasterURL: origin, Role: role, Token: testLocalToken})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := protectSessionPath(path, false); err != nil {
		t.Fatal(err)
	}
}

func TestLocalClusterCommandsDoNotRequireLogin(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			_, executable := discoveryTestPaths(t)
			t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
			t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
			seen := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen[r.Method+" "+r.URL.Path]++
				if r.Header.Get("Authorization") != "Bearer "+testLocalToken || r.Header.Get("X-Agent-Key") != "" {
					t.Error("local CLI access not authenticated independently")
					w.WriteHeader(401)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/master/status" {
					io.WriteString(w, `{"success":true,"tasks":[],"hasMore":false}`)
				} else {
					io.WriteString(w, `{"success":true,"data":{},"user":{"username":"local-cli","role":"admin"}}`)
				}
			}))
			defer server.Close()
			writeTestConnection(t, executable, role, server.URL)
			writeTestLocalAuth(t, executable+".local-auth.json", role, server.URL)
			if err := saveSession(server.URL, "expired-account-session"); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"whoami"}, {"task", "list"}, {"node", "network"}, {"node", "network", "--agent-url", "http://10.0.0.8:1685"}} {
				var out, stderr bytes.Buffer
				if code := runCLI(append([]string{"--json"}, args...), strings.NewReader(""), &out, &stderr); code != 0 {
					t.Fatal(args, code, out.String(), stderr.String())
				}
				if strings.Contains(out.String()+stderr.String(), testLocalToken) {
					t.Fatal("credential appeared in CLI output")
				}
			}
			if seen["POST /api/auth/login"] != 0 || seen["PUT /api/master/network"] != 1 || seen["GET /api/auth/current"] != 1 || seen["GET /api/master/status"] != 1 {
				t.Fatal("commands did not exercise expected API", seen)
			}
		})
	}
}

func TestLocalCLIAuthStaysWithItsOriginAndAccountCommands(t *testing.T) {
	_, executable := discoveryTestPaths(t)
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	var expected string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != expected {
			t.Errorf("wrong credential selected for %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if expected == "" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"success":false,"message":"Sign in required"}`)
			return
		}
		io.WriteString(w, `{"success":true,"user":{"username":"admin"}}`)
	}))
	defer server.Close()
	writeTestConnection(t, executable, "master", "http://127.0.0.1:1")
	writeTestLocalAuth(t, executable+".local-auth.json", "master", "http://127.0.0.1:1")
	var out, stderr bytes.Buffer
	if code := runCLI([]string{"--host", server.URL, "--json", "whoami"}, strings.NewReader(""), &out, &stderr); code != 1 {
		t.Fatal("another Master must still require account login", code)
	}
	if err := saveSession(server.URL, "remote-account-session"); err != nil {
		t.Fatal(err)
	}
	expected = "Bearer remote-account-session"
	if code := runCLI([]string{"--host", server.URL, "--json", "whoami"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatal(code, out.String(), stderr.String())
	}
	writeTestConnection(t, executable, "master", server.URL)
	writeTestLocalAuth(t, executable+".local-auth.json", "master", server.URL)
	expected = "Bearer " + testLocalToken
	if code := runCLI([]string{"--host", server.URL, "--json", "whoami"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	expected = "Bearer remote-account-session"
	if code := runCLI([]string{"--json", "logout"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if _, err := os.Stat(executable + ".local-auth.json"); err != nil {
		t.Fatal("logout removed system access", err)
	}
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "explicit-user-token")
	expected = "Bearer explicit-user-token"
	if code := runCLI([]string{"--json", "whoami"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if calls != 5 {
		t.Fatal(calls)
	}
}

func TestLocalAuthRejectsUnsafeFilesAndMismatchedOrigins(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "mismatched", "malformed", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dir, executable := discoveryTestPaths(t)
			origin := "http://127.0.0.1:1685"
			writeTestConnection(t, executable, "agent", origin)
			path := executable + ".local-auth.json"
			writeTestLocalAuth(t, path, "agent", origin)
			switch kind {
			case "public":
				if runtime.GOOS == "windows" {
					t.Skip("covered by Windows ACL tests")
				}
				os.Chmod(path, 0644)
			case "symlink":
				target := filepath.Join(dir, "secret.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skip(err)
				}
			case "mismatched":
				writeTestLocalAuth(t, path, "agent", "http://127.0.0.1:1686")
			case "malformed":
				os.WriteFile(path, []byte("invalid"), 0600)
			case "oversized":
				os.WriteFile(path, bytes.Repeat([]byte("x"), 65537), 0600)
			}
			if token, err := loadLocalCLIAuth(origin); err == nil || token != "" {
				t.Fatal("unsafe local credential accepted", kind)
			}
			if token, err := loadLocalCLIAuth("https://other.example"); err != nil || token != "" {
				t.Fatal("unrelated local credential affected remote access", err)
			}
		})
	}
}

func TestLocalAuthContainerAndManagedDataDiscovery(t *testing.T) {
	for _, kind := range []string{"container", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := discoveryTestPaths(t)
			origin := "http://127.0.0.1:1685"
			data := filepath.Join(dir, "data")
			if err := os.Mkdir(data, 0700); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]string{"masterUrl": origin, "role": "master"})
			os.WriteFile(filepath.Join(data, "cli-cluster.json"), raw, 0644)
			writeTestLocalAuth(t, filepath.Join(data, "cli-local-auth.json"), "master", origin)
			if kind == "container" {
				cliContainerPaths = func() []string { return []string{filepath.Join(data, "cli-cluster.json")} }
			} else if runtime.GOOS == "windows" {
				raw, _ := json.Marshal(map[string]string{"role": "master", "dataDir": data})
				os.WriteFile(filepath.Join(dir, "install.json"), raw, 0644)
			} else {
				os.WriteFile(filepath.Join(dir, "install.tsv"), []byte("role\tmaster\ndataDir\t"+data+"\n"), 0644)
			}
			if token, err := loadLocalCLIAuth(origin); err != nil || token != testLocalToken {
				t.Fatal("missing local data access", err)
			}
		})
	}
}
