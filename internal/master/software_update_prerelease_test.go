package master

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

func TestSoftwareVersionComparisonUsesSemVerPrecedence(t *testing.T) {
	for _, tc := range []struct {
		target, current string
		want            bool
	}{
		{"4.1.29", "4.1.29-beta.1", true}, {"4.1.29-beta.2", "4.1.29-beta.1", true}, {"4.1.29-rc.1", "4.1.29-beta.9", true},
		{"4.1.29-beta.1", "4.1.29", false}, {"4.1.29-beta.1", "4.1.28", true}, {"v4.1.29", "4.1.29-rc.1", true},
	} {
		if got := newerSoftware(tc.target, tc.current); got != tc.want {
			t.Fatalf("%s > %s: %v", tc.target, tc.current, got)
		}
	}
	if command := agentUpdateCommand("4.1.29"); len(command) != 1 || command["version"] != "4.1.29" {
		t.Fatal("stable Agent command must keep the legacy payload", command)
	}
	if command := agentUpdateCommand("4.1.29-beta.1"); command["allowPrerelease"] != true {
		t.Fatal("prerelease Agent command lost its explicit flag", command)
	}
}

func TestSoftwareUpdateOffersStableAndRequiresExplicitPrerelease(t *testing.T) {
	f := newAgentHTTPFixture(t)
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.28", Supported: true}, release: softwareupdate.Release{Version: "4.1.29"}, published: map[string]bool{"4.1.30-beta.1": true, "4.1.29": true}}
	f.master.options.Updater = u
	call := func(method, path string, body any) (int, map[string]any) {
		return request(t, f.http.Client(), method, f.http.URL+path, f.admin, body)
	}
	if status, _ := call("POST", "/api/master/software-updates/check", map[string]any{}); status != 200 {
		t.Fatal(status)
	}
	if status, body := call("GET", "/api/master/software-updates", nil); status != 200 || body["data"].(map[string]any)["available"] != true {
		t.Fatal("stable node was not offered the latest stable release", status, body)
	}
	status, body := call("POST", "/api/master/software-updates/master", map[string]any{"version": "4.1.30-beta.1"})
	if status != 400 || !strings.Contains(fmt.Sprint(body), "explicit prerelease confirmation") {
		t.Fatal("prerelease accepted without opt-in", status, body)
	}
	code, _, localized, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/master", f.admin, "zh-CN", map[string]any{"version": "4.1.30-beta.1", "allowPrerelease": false})
	if err != nil || code != 400 || !strings.Contains(fmt.Sprint(localized), "预发布版本需明确确认后才能安装") {
		t.Fatal("prerelease opt-in error is not localized", code, localized, err)
	}
	for _, tc := range []struct {
		version string
		status  int
	}{{"4.1.30-beta.9", 502}, {"4.1.27-beta.1", 409}, {"4.1.28", 409}, {"4.1.30-beta.01", 400}, {"latest", 400}} {
		if status, body := call("POST", "/api/master/software-updates/master", map[string]any{"version": tc.version, "allowPrerelease": true}); status != tc.status {
			t.Fatalf("%s: status=%d body=%v", tc.version, status, body)
		}
	}
	if status, body := call("POST", "/api/master/software-updates/master", map[string]any{"version": "4.1.30-beta.1", "allowPrerelease": true}); status != 202 {
		t.Fatal(status, body)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !reflect.DeepEqual(u.started, []string{"4.1.30-beta.1"}) || !reflect.DeepEqual(u.allowed, []bool{true}) || !reflect.DeepEqual(u.resolved, []string{"4.1.30-beta.9", "4.1.30-beta.1"}) {
		t.Fatal("unexpected update calls", u.started, u.allowed, u.resolved)
	}
}

func TestPrereleaseMasterIsOfferedNewerStable(t *testing.T) {
	for _, tc := range []struct {
		latest string
		want   bool
	}{{"4.1.29", true}, {"4.1.28", false}, {"4.1.30", true}} {
		t.Run(tc.latest, func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.29-beta.1", Supported: true}, release: softwareupdate.Release{Version: tc.latest}}
			f.master.options.Updater = u
			if status, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/check", f.admin, map[string]any{}); status != 200 {
				t.Fatal(status)
			}
			_, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/software-updates", f.admin, nil)
			if body["data"].(map[string]any)["available"] != tc.want {
				t.Fatalf("latest %s offered=%v", tc.latest, body["data"].(map[string]any)["available"])
			}
			if !tc.want {
				return
			}
			// The normal path installs the checked stable release without any flag.
			if status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/master", f.admin, map[string]any{"version": tc.latest}); status != 202 {
				t.Fatal(status, body)
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			if !reflect.DeepEqual(u.allowed, []bool{false}) {
				t.Fatal(u.allowed)
			}
		})
	}
}

func TestAgentsFollowOnlyTheMastersOwnPrerelease(t *testing.T) {
	f := newAgentHTTPFixture(t)
	master := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.29-beta.1", Supported: true}}
	f.master.options.Updater = master
	capable := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.28", Supported: true, PrereleaseUpdates: true}}
	legacy := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.28", Supported: true}}
	nc := nodeTestAgentWithUpdater(t, f, "beta-capable", capable)
	nl := nodeTestAgentWithUpdater(t, f, "beta-legacy", legacy)
	call := func(address, version string) {
		t.Helper()
		if status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/agents/"+address, f.admin, map[string]any{"version": version}); status != 202 {
			t.Fatal(status, body)
		}
	}
	if status, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/agents/"+nc.address, f.admin, map[string]any{"version": "4.1.29-beta.2"}); status != 409 {
		t.Fatal("Agent was sent a prerelease other than the Master's version", status)
	}
	call(nl.address, "4.1.29-beta.1")
	f.master.stepSoftwareQueue(context.Background())
	q, _ := f.master.softwareQueue()
	if q.Items[0].Phase != "SKIPPED" || q.Items[0].Reason != "PRERELEASE_UNSUPPORTED" {
		t.Fatal("legacy Agent was not skipped clearly", q)
	}
	f.master.stepSoftwareQueue(context.Background())
	call(nc.address, "4.1.29-beta.1")
	f.master.stepSoftwareQueue(context.Background())
	capable.mu.Lock()
	capable.info.Version, capable.status.Phase = "4.1.29-beta.1", "COMPLETED"
	capable.mu.Unlock()
	f.master.stepSoftwareQueue(context.Background())
	f.master.stepSoftwareQueue(context.Background())
	if q, _ = f.master.softwareQueue(); q.Phase != "COMPLETED" {
		t.Fatal(q)
	}
	// Beta -> stable: once the Master is stable again, Agents follow it with
	// the legacy stable payload and no prerelease flag.
	master.mu.Lock()
	master.info.Version = "4.1.29"
	master.mu.Unlock()
	_, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/software-updates", f.admin, nil)
	for _, raw := range body["data"].(map[string]any)["agents"].([]any) {
		row := raw.(map[string]any)
		if row["address"] == nc.address && row["updateAvailable"] != true {
			t.Fatal("prerelease Agent was not offered the stable Master version", row)
		}
	}
	call(nc.address, "4.1.29")
	f.master.stepSoftwareQueue(context.Background())
	capable.mu.Lock()
	defer capable.mu.Unlock()
	legacy.mu.Lock()
	defer legacy.mu.Unlock()
	if len(legacy.started) != 0 || !reflect.DeepEqual(capable.started, []string{"4.1.29-beta.1", "4.1.29"}) || !reflect.DeepEqual(capable.allowed, []bool{true, false}) {
		t.Fatal("unexpected Agent updates", legacy.started, capable.started, capable.allowed)
	}
}

func TestStableMasterNeverSendsPrereleaseToAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.28", Supported: true}}
	agent := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.28-beta.1", Supported: true, PrereleaseUpdates: true}}
	node := nodeTestAgentWithUpdater(t, f, "stable-follow", agent)
	if status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/agents/"+node.address, f.admin, map[string]any{"version": "4.1.28"}); status != 202 {
		t.Fatal(status, body)
	}
	f.master.stepSoftwareQueue(context.Background())
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !reflect.DeepEqual(agent.started, []string{"4.1.28"}) || !reflect.DeepEqual(agent.allowed, []bool{false}) {
		t.Fatal(agent.started, agent.allowed)
	}
}
