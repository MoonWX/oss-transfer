package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateVersionUsesStrictSemVer(t *testing.T) {
	for value, prerelease := range map[string]bool{"4.1.29": false, "v4.1.29": false, "4.1.29-beta.1": true, "4.1.29-rc.1": true, "1.0.0-0a.x-y": true} {
		version, pre, err := updateVersion(value)
		if err != nil || pre != prerelease || version != strings.TrimPrefix(value, "v") {
			t.Fatalf("%s: %q %v %v", value, version, pre, err)
		}
	}
	for _, value := range []string{"", "latest", "4.1", "04.1.29", "4.1.29-", "4.1.29-beta..1", "4.1.29-beta.01", "4.1.29+build", "4.1.29-be_ta", "4.1.0-dev", "vv4.1.29", "1234567890.0.0", "4.1.29-" + strings.Repeat("a", 33), "4.1.29-a.b.c.d.e.f.g.h.i"} {
		if _, _, err := updateVersion(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestUpdatePrereleaseRequiresExplicitFlag(t *testing.T) {
	for _, args := range [][]string{
		{"master", "--version", "4.1.29-beta.1", "--yes"},
		{"agent", "--worker", "node-a", "--version", "4.1.29-beta.1", "--yes"},
		{"agents", "--all", "--version", "v4.1.29-rc.1", "--yes"},
	} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("prerelease without flag made request: %s %s", r.Method, r.URL)
		})
		_, err := runMaintenanceCommand(client, "update", args)
		if err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "--allow-prerelease") {
			t.Fatalf("%v: %v", args, err)
		}
		for locale, want := range map[cliLocale]string{localeEN: "add --allow-prerelease to confirm", localeJA: "--allow-prerelease も指定"} {
			if message := localizedCLIError(locale, err); !strings.Contains(message, want) {
				t.Fatalf("%v: %s", locale, message)
			}
		}
	}
	code, _, stderr := runMockCLI(t, []string{"--lang", "en", "update", "master", "--version", "4.1.29-beta.1", "--yes"}, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected network")
	})
	if code != 2 || !strings.Contains(stderr, "--allow-prerelease") {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
}

func TestUpdatePrereleaseSubmissions(t *testing.T) {
	for _, test := range []struct {
		args       []string
		path       string
		body       map[string]interface{}
		masterInfo string
	}{
		{[]string{"master", "--version", "4.1.29-beta.1", "--allow-prerelease", "--yes"}, "/api/master/software-updates/master", map[string]interface{}{"version": "4.1.29-beta.1", "allowPrerelease": true}, ""},
		{[]string{"master", "--version", "4.1.29", "--yes"}, "/api/master/software-updates/master", map[string]interface{}{"version": "4.1.29"}, ""},
		// The prerelease flag is not sent for a stable version.
		{[]string{"master", "--version", "4.1.29", "--allow-prerelease", "--yes"}, "/api/master/software-updates/master", map[string]interface{}{"version": "4.1.29"}, ""},
		// Agents target the Master's version; the server validates it, so the
		// Agent request keeps its existing shape.
		{[]string{"agent", "--worker", "node-a", "--version", "4.1.29-beta.1", "--allow-prerelease", "--yes"}, "/api/master/software-updates/agents/node-a", map[string]interface{}{"version": "4.1.29-beta.1"}, ""},
		{[]string{"agents", "--all", "--yes"}, "/api/master/software-updates/agents", map[string]interface{}{"version": "4.1.29-beta.1"}, "4.1.29-beta.1"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			posts := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && test.masterInfo != "" {
					adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": test.masterInfo}}}})
					return
				}
				posts++
				if r.Method != "POST" || r.URL.Path != test.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if body := adminTestBody(t, r); !reflect.DeepEqual(body, test.body) {
					t.Fatalf("body %#v, want %#v", body, test.body)
				}
				w.WriteHeader(http.StatusAccepted)
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"phase": "QUEUED"}})
			})
			if _, err := runMaintenanceCommand(client, "update", test.args); err != nil || posts != 1 {
				t.Fatalf("posts=%d err=%v", posts, err)
			}
		})
	}
}

func TestUpdateHelpDocumentsPrereleaseFlag(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		if help := helpForLocale([]string{"update"}, locale); !strings.Contains(help, "--allow-prerelease") {
			t.Fatalf("%v help lacks the flag:\n%s", locale, help)
		}
	}
}
