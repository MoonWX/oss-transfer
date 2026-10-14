package servicehost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationRejectsUnsafeOrAmbiguousFields(t *testing.T) {
	data := t.TempDir()
	config := Config{ServiceName: "qa-master", DataDirectory: data, Listen: "127.0.0.1:1685", ReadyFile: filepath.Join(data, "runtime", "ready"), LogFile: filepath.Join(data, "logs", "service.log")}
	path := filepath.Join(data, "config.json")
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, "master"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(string(raw), `127.0.0.1:1685`, `127.0.0.1:http`, 1),
		strings.Replace(string(raw), `qa-master`, "qa-master\\nExecStart=/bad", 1),
		string(raw) + ` {}`,
		strings.TrimSuffix(string(raw), "}") + `,"joinToken":"secret"}`,
	} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path, "master"); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestReadinessRequiresThisInvocationAndManagerPID(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "runtime", "ready")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 64)
	if err := os.WriteFile(path+".request", []byte(nonce), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale record"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := prepare(Options{Name: "qa", Role: "master", Version: "4.1.0-test", ReadyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale readiness retained")
	}
	if err := r.ready(); err != nil {
		t.Fatal(err)
	}
	if err := CheckReady(path, nonce, "master", "4.1.0-test", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		nonce, role, version string
		pid                  int
	}{
		{strings.Repeat("b", 64), "master", "4.1.0-test", os.Getpid()},
		{nonce, "agent", "4.1.0-test", os.Getpid()},
		{nonce, "master", "old-version", os.Getpid()},
		{nonce, "master", "4.1.0-test", os.Getpid() + 1},
	} {
		if CheckReady(path, check.nonce, check.role, check.version, check.pid) == nil {
			t.Fatal("stale or foreign process accepted")
		}
	}
	r.close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("shutdown retained readiness")
	}
}

func TestReadinessCannotFollowApplicationSymlink(t *testing.T) {
	data := t.TempDir()
	out := t.TempDir()
	link := filepath.Join(data, "runtime")
	if err := os.Symlink(out, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := prepare(Options{Name: "qa", Role: "master", Version: "test", ReadyFile: filepath.Join(link, "ready")}); err == nil {
		t.Fatal("readiness followed link")
	}
}
