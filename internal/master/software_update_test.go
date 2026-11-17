package master

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

type fakeSoftwareUpdater struct {
	mu      sync.Mutex
	info    softwareupdate.Info
	status  softwareupdate.Status
	release softwareupdate.Release
	started []string
	allowed []bool
	// published limits Resolve to these versions when non-nil.
	published map[string]bool
	resolved  []string
	err       error
}

func TestSoftwareUpdateQueueSurvivesMasterRestartAndSerializesAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	master := &fakeSoftwareUpdater{info: softwareupdate.Info{Version: "4.1.15", Role: "master", Supported: true}}
	f.master.options.Updater = master
	ua := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}}
	ub := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}}
	a, b := nodeTestAgentWithUpdater(t, f, "upgrade-a", ua), nodeTestAgentWithUpdater(t, f, "upgrade-b", ub)
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/software-updates/agents", f.admin, map[string]string{"version": "4.1.15"})
	if status != 202 {
		t.Fatal(status, body)
	}
	queue, err := f.master.softwareQueue()
	if err != nil || len(queue.Items) != 2 {
		t.Fatal(queue, err)
	}
	first, second := ua, ub
	if queue.Items[0].Address == b.address {
		first, second = ub, ua
	}
	f.master.stepSoftwareQueue(context.Background())
	assertStarts := func(u *fakeSoftwareUpdater, want int) {
		t.Helper()
		u.mu.Lock()
		defer u.mu.Unlock()
		if len(u.started) != want {
			t.Fatalf("update starts = %v, want %d", u.started, want)
		}
	}
	assertStarts(first, 1)
	assertStarts(second, 0)
	if !f.master.softwareUpdateBlocksNode(queue.Items[0].NodeID) || f.master.softwareUpdateBlocksNode(queue.Items[1].NodeID) {
		t.Fatal("admission must block only the node currently upgrading")
	}
	member, _ := f.master.nodeByAddress(queue.Items[0].Address)
	if err := f.master.agentCall(context.Background(), member.Session(), "child_accept", true, map[string]any{}, nil); !agentStatus(err, 409) {
		t.Fatal("upgrade admitted new transfer work", err)
	}
	var info agentSoftware
	if err := f.master.agentCall(context.Background(), member.Session(), "software_info", false, struct{}{}, &info); err != nil {
		t.Fatal("upgrade blocked status read", err)
	}
	// Reopen the real Master DB and reverse Agent channels; no in-memory queue
	// or browser request remains alive to drive or reconstruct this operation.
	a.close(t)
	b.close(t)
	f.close()
	f.open(t)
	f.master.options.Updater = master
	a.open(t)
	b.open(t)
	restored, err := f.master.softwareQueue()
	if err != nil || restored.ID != queue.ID || restored.Items[0].Phase != "UPDATING" {
		t.Fatal("lost persisted queue", restored, err)
	}
	f.master.stepSoftwareQueue(context.Background())
	assertStarts(first, 1)
	assertStarts(second, 0)
	first.mu.Lock()
	first.info.Version, first.status.Phase = "4.1.15", "APPLYING"
	first.mu.Unlock()
	f.master.stepSoftwareQueue(context.Background())
	assertStarts(second, 0) // Reconnected process alone is not installer success.
	first.mu.Lock()
	first.status.Phase = "COMPLETED"
	first.mu.Unlock()
	f.master.stepSoftwareQueue(context.Background())
	f.master.stepSoftwareQueue(context.Background())
	assertStarts(second, 1)
	if f.master.softwareUpdateBlocksNode(queue.Items[0].NodeID) {
		t.Fatal("completed node remained fenced")
	}
	second.mu.Lock()
	second.status.Phase, second.status.Error = "FAILED", "installer rolled back"
	second.mu.Unlock()
	f.master.stepSoftwareQueue(context.Background())
	restored, _ = f.master.softwareQueue()
	if restored.Phase != "FAILED" || restored.Items[1].Error != "installer rolled back" {
		t.Fatal(restored)
	}
}

func TestSoftwareUpdateQueueStopsAfterRejectedUpgradeAndExplainsLegacy(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Version: "4.1.15", Supported: true}}
	updater := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}, err: errors.New("installer metadata changed")}
	nodeTestAgentWithUpdater(t, f, "failed-a", updater)
	nodeTestAgentWithUpdater(t, f, "failed-b", updater)
	q, err := f.master.enqueueAgentUpdates(context.Background(), "4.1.15", "")
	if err != nil {
		t.Fatal(err)
	}
	f.master.stepSoftwareQueue(context.Background())
	q, err = f.master.softwareQueue()
	if err != nil || q.Phase != "FAILED" || q.Items[0].Phase != "FAILED" || q.Items[1].Reason != "PREVIOUS_FAILED" {
		t.Fatal(q, err)
	}
	legacy := nodeTestAgent(t, f, "legacy")
	f.master.options.AgentDockerImage = "registry.example/transfer:4.1.15"
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/software-updates", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	// The console builds the container Agent update commands from this image.
	if image := body["data"].(map[string]any)["agentImage"]; image != "registry.example/transfer:4.1.15" {
		t.Fatal("container Agent image missing from the update status", image)
	}
	for _, raw := range body["data"].(map[string]any)["agents"].([]any) {
		row := raw.(map[string]any)
		if row["address"] == legacy.address && (row["unavailable"] == true || row["upgradeReason"] != "LEGACY") {
			t.Fatal("reachable legacy Agent incorrectly shown offline", row)
		}
	}
}

func TestSoftwareUpdateQueueDistinguishesOldFailureAndBoundsMissingResult(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Version: "4.1.15", Supported: true}}
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}, status: softwareupdate.Status{ID: "previous-failure", Version: "4.1.15", Phase: "FAILED"}}
	node := nodeTestAgentWithUpdater(t, f, "restart-before-dispatch", u)
	q, err := f.master.enqueueAgentUpdates(context.Background(), "4.1.15", node.address)
	if err != nil {
		t.Fatal(err)
	}
	// Master persisted admission but died before dispatch. The old failed
	// helper must not be mistaken for a failure of this new attempt.
	q.Items[0].Phase, q.Items[0].PreviousID, q.Items[0].StartedAt = "UPDATING", "previous-failure", time.Now()
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	f.master.stepSoftwareQueue(context.Background())
	q, _ = f.master.softwareQueue()
	u.mu.Lock()
	starts := len(u.started)
	u.mu.Unlock()
	if starts != 1 || q.Phase != "RUNNING" || q.Items[0].UpdateID != "update-1" {
		t.Fatal("previous failure consumed a new attempt", q, starts)
	}
	// A new binary with a missing helper result cannot hold the queue forever.
	u.mu.Lock()
	u.info.Version, u.status = "4.1.15", softwareupdate.Status{Phase: "IDLE"}
	u.mu.Unlock()
	q.Items[0].StartedAt = time.Now().Add(-2 * time.Hour)
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	f.master.stepSoftwareQueue(context.Background())
	q, _ = f.master.softwareQueue()
	if q.Phase != "FAILED" || q.Items[0].Reason != "TIMEOUT" {
		t.Fatal("missing helper result never timed out", q)
	}
	// Conversely, a Master that was down for more than an hour must accept
	// the matching helper's persisted success instead of timing it out first.
	q.Phase, q.Items[0].Phase, q.Items[0].UpdateID = "RUNNING", "UPDATING", "update-1"
	u.mu.Lock()
	u.status = softwareupdate.Status{ID: "update-1", Version: "4.1.15", Phase: "COMPLETED"}
	u.mu.Unlock()
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	f.master.stepSoftwareQueue(context.Background())
	q, _ = f.master.softwareQueue()
	if q.Items[0].Phase != "COMPLETED" {
		t.Fatal("Master downtime hid a confirmed successful update", q)
	}
}

func TestSoftwareUpdateQueueRetriesBriefAgentDisconnection(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Version: "4.1.15", Supported: true}}
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}}
	node := nodeTestAgentWithUpdater(t, f, "reconnect", u)
	if _, err := f.master.enqueueAgentUpdates(context.Background(), "4.1.15", node.address); err != nil {
		t.Fatal(err)
	}
	node.close(t)
	f.master.stepSoftwareQueue(context.Background())
	q, _ := f.master.softwareQueue()
	if q.Items[0].Phase != "QUEUED" || q.Items[0].Reason != "RECONNECTING" {
		t.Fatal("first timeout permanently skipped a reconnecting Agent", q)
	}
	node.open(t)
	f.master.stepSoftwareQueue(context.Background())
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.started) != 1 {
		t.Fatal("reconnected Agent was not updated", u.started)
	}
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
func (u *fakeSoftwareUpdater) Resolve(_ context.Context, version string, allowPrerelease bool) (softwareupdate.Release, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.resolved = append(u.resolved, version)
	if u.published != nil && !u.published[version] {
		return softwareupdate.Release{}, errors.New("release not found")
	}
	if softwareupdate.IsPrerelease(version) && !allowPrerelease {
		return softwareupdate.Release{}, softwareupdate.ErrPrereleaseNotAllowed
	}
	return softwareupdate.Release{Version: version}, nil
}
func (u *fakeSoftwareUpdater) Start(_ context.Context, version string, allowPrerelease bool) (softwareupdate.Status, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.started = append(u.started, version)
	u.allowed = append(u.allowed, allowPrerelease)
	u.status = softwareupdate.Status{ID: fmt.Sprintf("update-%d", len(u.started)), Version: version, Phase: "QUEUED"}
	return u.status, u.err
}

func TestSoftwareVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		target, current string
		want            bool
	}{
		{"4.1.15", "4.1.14", true}, {"4.1.10", "4.1.9", true}, {"4.1.14", "4.1.14", false}, {"4.1.9", "4.1.14", false}, {"4.2.0", "4.1.99", true}, {"4.1.15-rc.1", "4.1.14", true}, {"4.1.15-rc.1", "4.1.15", false}, {"4.1.15", "4.1.0-dev", false}, {"garbage", "4.1.14", false},
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
	for _, path := range []string{"/api/master/software-updates/check", "/api/master/software-updates/master", "/api/master/software-updates/agents/unknown", "/api/master/software-updates/agents"} {
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
	agent := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.13", Supported: true}}
	node := nodeTestAgentWithUpdater(t, f, "software-node", agent)
	path := "/api/master/software-updates/agents/" + node.address
	if status, _ := call("POST", path, map[string]string{"version": "4.1.15"}); status != 409 {
		t.Fatal("Agent advanced ahead of Master", status)
	}
	if status, body := call("POST", path, map[string]string{"version": "4.1.14"}); status != 202 {
		t.Fatal("Agent update dispatch", status, body)
	}
	f.master.stepSoftwareQueue(context.Background())
	if status, body := call("GET", "/api/master/software-updates", nil); status != 200 {
		t.Fatal(status, body)
	} else {
		data := body["data"].(map[string]any)
		nodes := data["agents"].([]any)
		if len(nodes) != 1 || nodes[0].(map[string]any)["info"].(map[string]any)["version"] != "4.1.13" {
			t.Fatal("reported target as installed", body)
		}
	}
	if status, body := call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.15"}); status != 409 {
		t.Fatal("Master update raced Agent queue", status, body)
	}
	agent.mu.Lock()
	agent.info.Version, agent.status.Phase = "4.1.14", "COMPLETED"
	agent.mu.Unlock()
	f.master.stepSoftwareQueue(context.Background())
	f.master.stepSoftwareQueue(context.Background())
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

// A failed version check keeps the reason each release source gave, says
// whether checking again can help, and records when it failed: the console
// does not check again on its own soon after.
func TestVersionCheckFailureKeepsEachSourceReason(t *testing.T) {
	f := newAgentHTTPFixture(t)
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.14", Supported: true}}
	f.master.options.Updater = u
	unanswered := func(source, address string) error {
		return fmt.Errorf("%s: %w", source, &url.Error{Op: "Get", URL: address, Err: errors.New("dial tcp: lookup host: no such host")})
	}
	offline := errors.Join(unanswered("github", "https://api.github.com/repos/MoonWX/oss-transfer/releases/latest"), unanswered("jdoss", "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/release.json"))
	refused := errors.Join(errors.New("github: Update download returned HTTP 403"), unanswered("jdoss", "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/release.json"))
	call := func(method, path, language string) (int, map[string]any) {
		t.Helper()
		var body any
		if method == "POST" {
			body = map[string]any{}
		}
		code, _, result, err := localizedRequest(f.http.Client(), method, f.http.URL+path, f.admin, language, body)
		if err != nil {
			t.Fatal(err)
		}
		return code, result
	}
	for _, test := range []struct {
		err            error
		language, want string
	}{
		{offline, "zh-CN", "Master 无法连接 GitHub 和京东云 OSS，因此无法检测新版本。在能够访问其中之一之前，重新检测不会成功；也可以使用离线安装包升级（见文档“离线安装”）"},
		{offline, "", versionSourcesUnreachable},
		{refused, "zh-CN", "版本检测失败：发布源未返回有效的版本信息"},
		{refused, "ja-JP", "バージョンの確認に失敗しました。リリース元から有効なバージョン情報を取得できませんでした"},
	} {
		u.mu.Lock()
		u.err = test.err
		u.mu.Unlock()
		code, body := call("POST", "/api/master/software-updates/check", test.language)
		if code != 502 || body["message"] != test.want || body["detail"] != test.err.Error() {
			t.Fatalf("%v in %q: %d %v", test.err, test.language, code, body)
		}
		code, body = call("GET", "/api/master/software-updates", test.language)
		data, _ := body["data"].(map[string]any)
		failed, err := time.Parse(time.RFC3339Nano, fmt.Sprint(data["checkFailedAt"]))
		if code != 200 || data["checkError"] != test.want || data["checkErrorDetail"] != test.err.Error() || err != nil || failed.IsZero() {
			t.Fatalf("stored failure of %v in %q: %d %v", test.err, test.language, code, data)
		}
	}
	// A successful check clears the failure.
	u.mu.Lock()
	u.err, u.release = nil, softwareupdate.Release{Version: "4.1.15"}
	u.mu.Unlock()
	if code, body := call("POST", "/api/master/software-updates/check", ""); code != 200 {
		t.Fatal(code, body)
	}
	_, body := call("GET", "/api/master/software-updates", "")
	data := body["data"].(map[string]any)
	if data["checkError"] != "" || data["checkErrorDetail"] != nil || data["checkFailedAt"] != nil {
		t.Fatal("a successful check kept the failure", data)
	}
}

// A failed online update stops describing a node once the node runs the
// version that update targeted, for example after an upgrade with the
// installer. A failure for a version the node has not reached stays visible.
func TestSoftwareUpdatesHideFailuresTheNodeHasSincePassed(t *testing.T) {
	f := newAgentHTTPFixture(t)
	master := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.16", Supported: true}, status: softwareupdate.Status{ID: "m1", Version: "4.1.16", Phase: "FAILED", Error: "Native upgrade failed"}}
	f.master.options.Updater = master
	upgraded := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.16", Supported: true}, status: softwareupdate.Status{ID: "a1", Version: "4.1.15", Phase: "FAILED", Error: "Agent update did not finish"}}
	behind := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.14", Supported: true}, status: softwareupdate.Status{ID: "b1", Version: "4.1.15", Phase: "FAILED", Error: "installer rolled back"}}
	upgradedNode := nodeTestAgentWithUpdater(t, f, "upgraded", upgraded)
	behindNode := nodeTestAgentWithUpdater(t, f, "behind", behind)
	read := func() (map[string]any, map[string]map[string]any) {
		t.Helper()
		status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/software-updates", f.admin, nil)
		if status != 200 {
			t.Fatal(status, body)
		}
		data := body["data"].(map[string]any)
		rows := map[string]map[string]any{}
		for _, raw := range data["agents"].([]any) {
			row := raw.(map[string]any)
			rows[row["address"].(string)] = row
		}
		return data["master"].(map[string]any)["update"].(map[string]any), rows
	}
	update, rows := read()
	if update["phase"] != "IDLE" || update["error"] != nil {
		t.Fatal("the Master's superseded failure is still reported", update)
	}
	if got := rows[upgradedNode.address]["update"].(map[string]any); got["phase"] != "IDLE" || got["error"] != nil {
		t.Fatal("an Agent's superseded failure is still reported", got)
	}
	if got := rows[behindNode.address]["update"].(map[string]any); got["phase"] != "FAILED" || got["error"] != "installer rolled back" {
		t.Fatal("a failure the Agent has not recovered from was hidden", got)
	}
	master.mu.Lock()
	master.status.Version = "4.1.17"
	master.mu.Unlock()
	if update, _ = read(); update["phase"] != "FAILED" || update["error"] != "Native upgrade failed" {
		t.Fatal("a failed update to a newer Master version was hidden", update)
	}
	// The persisted status stays as written; only its presentation changes.
	if status := master.Status(); status.Phase != "FAILED" {
		t.Fatal("reading the update page changed the stored status", status)
	}
}
