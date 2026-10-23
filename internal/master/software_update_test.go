package master

import (
	"context"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

type fakeSoftwareUpdater struct {
	mu      sync.Mutex
	info    softwareupdate.Info
	status  softwareupdate.Status
	release softwareupdate.Release
	started []string
	err     error
}

func (u *fakeSoftwareUpdater) Info() softwareupdate.Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.info
}
func (u *fakeSoftwareUpdater) Status() softwareupdate.Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}
func (u *fakeSoftwareUpdater) Check(context.Context) (softwareupdate.Release, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.release, u.err
}
func (u *fakeSoftwareUpdater) Start(_ context.Context, version string) (softwareupdate.Status, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.started = append(u.started, version)
	u.status = softwareupdate.Status{Version: version, Phase: "QUEUED"}
	return u.status, u.err
}

func TestSoftwareVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		target, current string
		want            bool
	}{
		{"4.1.15", "4.1.14", true}, {"4.1.10", "4.1.9", true}, {"4.1.14", "4.1.14", false}, {"4.1.9", "4.1.14", false}, {"4.2.0", "4.1.99", true}, {"4.1.15-rc.1", "4.1.14", false}, {"4.1.15", "4.1.0-dev", false}, {"garbage", "4.1.14", false},
	} {
		if got := newerSoftware(tc.target, tc.current); got != tc.want {
			t.Fatalf("%s > %s: %v", tc.target, tc.current, got)
		}
	}
}

func TestSoftwareUpdatesRequireAuthenticationAndCheckedVersion(t *testing.T) {
	f := newAgentHTTPFixture(t)
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.14", Supported: true}, release: softwareupdate.Release{Version: "4.1.15"}}
	f.master.options.Updater = u
	// All mutations use the existing administrator middleware. A caller cannot
	// turn the update endpoint into a generic URL/script execution endpoint.
	for _, path := range []string{"/api/master/software-updates/check", "/api/master/software-updates/master", "/api/master/software-updates/agents/unknown"} {
		status, _ := request(t, f.http.Client(), "POST", f.http.URL+path, "", map[string]any{})
		if status != 401 {
			t.Fatalf("unauth %s = %d", path, status)
		}
	}
	call := func(method, path string, body any) (int, map[string]any) {
		return request(t, f.http.Client(), method, f.http.URL+path, f.admin, body)
	}
	if status, _ := call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.15"}); status != 409 {
		t.Fatal("unchecked release accepted", status)
	}
	if status, _ := call("POST", "/api/master/software-updates/check", map[string]any{}); status != 200 {
		t.Fatal(status)
	}
	if status, _ := call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.99"}); status != 409 {
		t.Fatal("arbitrary release accepted", status)
	}
	if status, _ := call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.15", "url": "http://localhost/evil.sh"}); status != 400 {
		t.Fatal("arbitrary URL accepted", status)
	}
	node := nodeTestAgent(t, f, "software-node")
	agent := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.13", Supported: true}}
	node.service.Updater = agent
	path := "/api/master/software-updates/agents/" + node.address
	if status, _ := call("POST", path, map[string]string{"version": "4.1.15"}); status != 409 {
		t.Fatal("Agent advanced ahead of Master", status)
	}
	if status, body := call("POST", path, map[string]string{"version": "4.1.14"}); status != 202 {
		t.Fatal("Agent update dispatch", status, body)
	}
	if status, body := call("GET", "/api/master/software-updates", nil); status != 200 {
		t.Fatal(status, body)
	} else {
		data := body["data"].(map[string]any)
		nodes := data["agents"].([]any)
		if len(nodes) != 1 || nodes[0].(map[string]any)["info"].(map[string]any)["version"] != "4.1.13" {
			t.Fatal("reported target as installed", body)
		}
	}
	if status, body := call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.15"}); status != 202 {
		t.Fatal(status, body)
	}
	if status, _ := call("POST", path, map[string]string{"version": "4.1.14"}); status != 409 {
		t.Fatal("Agent update raced Master", status)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(u.started) != 1 || len(agent.started) != 1 || agent.started[0] != "4.1.14" {
		t.Fatal("unexpected update command", u.started, agent.started)
	}
}
