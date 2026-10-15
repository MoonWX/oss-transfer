package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func runLocalConfig(t *testing.T, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if code := runCLI(args, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("%v: code=%d out=%s stderr=%s", args, code, out.String(), stderr.String())
	}
	return out.String()
}

func TestCLILanguagePreferencePersistsAndCanReturnToAuto(t *testing.T) {
	clearLocaleEnvironment(t)
	t.Setenv("LANG", "ja_JP.UTF-8")
	// Local preferences must remain usable with an invalid service address and
	// a damaged login. They do not depend on the cluster or session loading.
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", "not-a-service-url")
	session, err := sessionPath("http://localhost:1685")
	if err != nil {
		t.Fatal(err)
	}
	if out := runLocalConfig(t, "config", "get", "language"); out != "auto\n" {
		t.Fatal(out)
	}
	if out := runLocalConfig(t, "config", "set", "language", "zh-CN"); out != "zh\n" {
		t.Fatal(out)
	}
	if err = os.WriteFile(session, []byte("broken session"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := runLocalConfig(t, "config", "get", "language", "--json"); out != "{\"language\":\"zh\"}\n" {
		t.Fatal(out)
	}
	t.Setenv("OSS_TRANSFER_MASTER_SERVER", "")
	if out := runLocalConfig(t, "--help"); !strings.Contains(out, "用法：") {
		t.Fatal("saved language was not used by a later command")
	}
	for _, test := range []struct {
		env, flag string
		want      cliLocale
	}{
		{"", "", localeZH},
		{"en", "", localeEN},
		{"en", "ja", localeJA},
		{"auto", "", localeJA},
		{"en", "auto", localeJA},
	} {
		t.Setenv("OSS_TRANSFER_LANG", test.env)
		var args []string
		if test.flag != "" {
			args = []string{"--lang", test.flag}
		}
		if got, err := resolveCLILocale(args); err != nil || got != test.want {
			t.Fatalf("env=%s flag=%s got=%s want=%s err=%v", test.env, test.flag, got, test.want, err)
		}
	}
	t.Setenv("OSS_TRANSFER_LANG", "")
	runLocalConfig(t, "config", "set", "language", "auto")
	if out := runLocalConfig(t, "--help"); !strings.Contains(out, "使い方:") {
		t.Fatal("auto did not restore the system language")
	}
	path, _ := cliPreferencesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var preference map[string]string
	if err := json.Unmarshal(data, &preference); err != nil || preference["language"] != "auto" || len(preference) != 1 {
		t.Fatalf("unexpected persisted settings: %s %v", data, err)
	}
}

func TestCLIConfigInvalidInputDoesNotOverwriteAndCorruptionIsRepairable(t *testing.T) {
	clearLocaleEnvironment(t)
	runLocalConfig(t, "config", "set", "language", "en")
	path, _ := cliPreferencesPath()
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"config", "set", "language", "unsupported"},
		{"config", "set", "lang", "ja"},
		{"config", "set", "language", "ja", "extra"},
		{"config", "get", "language", "--watch", "1"},
	} {
		var out, stderr bytes.Buffer
		if code := runCLI(args, nil, &out, &stderr); code != 2 {
			t.Fatalf("accepted invalid arguments %v: %d %s", args, code, stderr.String())
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid input changed saved settings")
		}
	}
	if err := os.WriteFile(path, []byte("invalid json"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runCLI([]string{"config", "get", "language"}, nil, &out, &stderr); code != 1 {
		t.Fatal("config get hid the corrupt file")
	}
	runLocalConfig(t, "config", "set", "language", "ja")
	if got, err := resolveCLILocale(nil); err != nil || got != localeJA {
		t.Fatal(got, err)
	}
}
