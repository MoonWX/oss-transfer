package master

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agentstate"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type nodeFixture struct {
	master                    *agentHTTPFixture
	path, address, credential string
	db                        *state.Store
	service                   *agentservice.Service
	cancel                    context.CancelFunc
	done                      chan error
	updater                   softwareupdate.Controller
}

func nodeTestAgent(t *testing.T, f *agentHTTPFixture, name string) *nodeFixture {
	return nodeTestAgentWithUpdater(t, f, name, nil)
}

func nodeTestAgentWithUpdater(t *testing.T, f *agentHTTPFixture, name string, updater softwareupdate.Controller) *nodeFixture {
	t.Helper()
	n := &nodeFixture{master: f, path: filepath.Join(t.TempDir(), "agent.db"), address: name + ".example", credential: agentCredential(t), updater: updater}
	if status, _ := f.join(t, f.invite(t, n.address), name, n.credential); status != 200 {
		t.Fatal("join failed", status)
	}
	n.open(t)
	t.Cleanup(func() { n.close(t) })
	return n
}
func (n *nodeFixture) open(t *testing.T) {
	t.Helper()
	var err error
	n.db, err = state.Open(n.path)
	if err != nil {
		t.Fatal(err)
	}
	n.service, err = agentservice.New(n.db, agentservice.Options{Updater: n.updater})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := agentstate.NewJournal(n.db, agentstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, n.master, n.credential), n.service, journal, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { n.done <- runner.Run(ctx) }()
}
func (n *nodeFixture) close(t *testing.T) {
	t.Helper()
	if n.cancel != nil {
		n.cancel()
		select {
		case <-n.done:
		case <-time.After(3 * time.Second):
			t.Error("node runner did not stop")
		}
		n.cancel = nil
	}
	if n.service != nil {
		if err := n.service.Close(context.Background()); err != nil {
			t.Error(err)
		}
		n.service = nil
	}
	if n.db != nil {
		if err := n.db.Close(); err != nil {
			t.Error(err)
		}
		n.db = nil
	}
}

// This crosses real HTTP, enrollment, the reverse command channel, durable
// command journal and two independent Agent databases. No RPC result is mocked.
func TestNodeHTTPSettingsReachEachAgentAndSurviveRestart(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a := nodeTestAgent(t, f, "node-a")
	b := nodeTestAgent(t, f, "node-b")
	call := func(method, path string, body any) (int, map[string]any) {
		return request(t, f.http.Client(), method, f.http.URL+path, f.admin, body)
	}
	status, body := call("POST", "/api/master/rate-limit/put", map[string]any{"qps": 100})
	if status != 200 || body["success"] != true || body["successCount"] != float64(2) {
		t.Fatal("cluster setting failed", status, body)
	}
	for _, agent := range []*nodeFixture{a, b} {
		snapshot, err := agent.service.Nodes.Snapshot()
		if err != nil || snapshot["put"].(map[string]any)["limitQps"] != float64(100) {
			t.Fatal("setting not applied on both Agents", err, snapshot)
		}
	}
	status, body = call("POST", "/api/master/rate-limit/put/"+a.address, map[string]any{"qps": 1400})
	if status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	roots := []string{filepath.Join(t.TempDir(), " shared files ")}
	status, body = call("PUT", "/api/master/nodes/"+a.address, map[string]any{"label": "Storage A", "advertisedLocalRoots": roots})
	if status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	if got := a.service.Nodes.Config().AdvertisedLocalRoots; len(got) != 1 || got[0] != roots[0] {
		t.Fatal("roots not saved verbatim", got)
	}
	status, _ = call("PUT", "/api/master/nodes/"+a.address, map[string]any{"address": "replacement.example"})
	if status != 409 {
		t.Fatal("credential-bound address mutated", status)
	}
	status, _ = call("PUT", "/api/master/nodes/"+a.address, map[string]any{"advertisedLocalRoots": []string{"relative"}})
	if status != 400 {
		t.Fatal("invalid local root accepted", status)
	}
	a.close(t)
	a.open(t)
	// The reopened Agent reports again only after it reconnects; wait for a
	// complete status that includes it rather than assuming it already has.
	var workers map[string]any
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status, body = call("GET", "/api/master/worker-status", nil)
		if status != 200 {
			t.Fatal(status, body)
		}
		workers, _ = body["workers"].(map[string]any)
		reopened, _ := workers[a.address].(map[string]any)
		if body["complete"] == true && reopened["rateLimit"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reopened Agent never reported a complete status", body)
		}
	}
	if len(workers) != 2 {
		t.Fatal("lost a registered Agent", workers)
	}
	worker := workers[a.address].(map[string]any)
	address, _ := worker["displayAddress"].(string)
	if worker["displayName"] != "Storage A" || net.ParseIP(address) == nil || worker["displayLabel"] != "Storage A ("+address+")" || worker["address"] != a.address {
		t.Fatal("node status failed to separate presentation from routing identity", worker)
	}
	arate := workers[a.address].(map[string]any)["rateLimit"].(map[string]any)["put"].(map[string]any)
	brate := workers[b.address].(map[string]any)["rateLimit"].(map[string]any)["put"].(map[string]any)
	if arate["limitQps"] != float64(1400) || arate["limitSource"] != "CONFIG" || brate["limitQps"] != float64(100) {
		t.Fatal("restart/single-node edit altered targets", arate, brate)
	}
	status, body = call("GET", "/api/master/local-nodes", nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	found := false
	for _, raw := range body["data"].(map[string]any)["nodes"].([]any) {
		row := raw.(map[string]any)
		if row["executionNodeIp"] == a.address {
			if row["displayLabel"] != worker["displayLabel"] {
				t.Fatal("local node selector and node status disagree", row, worker)
			}
			got := row["advertisedLocalRoots"].([]any)
			found = row["reachable"] == true && len(got) == 1 && got[0] == roots[0]
		}
	}
	if !found {
		t.Fatal("durable roots missing from local-node discovery", body)
	}
	status, body = call("GET", "/api/master/rate-limit/history?limit=50", nil)
	if status != 200 || body["complete"] != true || len(body["adjustments"].([]any)) != 3 {
		t.Fatal("per-node manual history incomplete", status, body)
	}
	var event map[string]any
	for _, raw := range body["adjustments"].([]any) {
		row := raw.(map[string]any)
		if row["workerIp"] == a.address {
			event = row
			break
		}
	}
	status, body = call("GET", "/api/master/rate-limit/adjustment?workerIp="+a.address+"&eventId="+event["id"].(string), nil)
	if status != 200 || body["processRestarted"] != true || body["historyComplete"] != true {
		t.Fatal("persistent detail unavailable after Agent restart", status, body)
	}
	if err := b.service.Nodes.ObserveResponse(nodecontrol.RequestContext{StatusCode: 503}); err != nil {
		t.Fatal(err)
	}
	status, body = call("GET", "/api/master/worker-status", nil)
	if status != 200 || body["workers"].(map[string]any)[b.address].(map[string]any)["rateLimit"].(map[string]any)["put"].(map[string]any)["state"] != "NORMAL" {
		t.Fatal("displayed warning without automatic reduction", body)
	}
}

// An Agent up to 4.1.46 rejects every node setting with the bare "invalid node
// setting", including a local storage limit it does not have and a recorded
// directory it cannot use. The Master names the node as too old for the limit
// and names the rejected directories; a current Agent names the setting
// itself. No reply sends the user to the concurrency settings or to retry.
func TestNodeSettingRejectionsNameTheSetting(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current := nodeTestAgent(t, f, "current")
	old := "old.example"
	code, session := f.join(t, f.invite(t, old), "old", agentCredential(t))
	if code != 200 {
		t.Fatal("join failed", code)
	}
	serveScanReleaseAgent(t, f, task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID}, func(command agenttransport.Command) (any, int) {
		switch command.Operation {
		case "node_rate_set", "node_config_set":
			return map[string]string{"code": "invalid_request", "message": "invalid node setting"}, 400
		}
		return map[string]string{"code": "unknown_operation"}, 404
	})
	results := func(body map[string]any) map[string]map[string]any {
		t.Helper()
		byNode := map[string]map[string]any{}
		for _, raw := range body["results"].([]any) {
			result := raw.(map[string]any)
			byNode[result["workerIp"].(string)] = result
		}
		return byNode
	}
	for _, test := range []struct{ language, want string }{
		{"", "This node's version does not support this limit; upgrade it first"},
		{"zh-CN", "此节点版本不支持该限速，请先升级"},
		{"ja-JP", "このノードのバージョンはこの制限に対応していません。先に更新してください"},
	} {
		status, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/rate-limit/localOps", f.admin, test.language, map[string]any{"qps": 300})
		if err != nil || status != 200 || body["code"] != "RATE_UPDATE_UNCONFIRMED" || body["failedCount"] != float64(1) {
			t.Fatalf("language %q: %d %v %v", test.language, status, body, err)
		}
		byNode := results(body)
		if byNode[old]["success"] != false || byNode[old]["code"] != "RATE_TYPE_UNSUPPORTED" || byNode[old]["message"] != test.want {
			t.Fatalf("language %q: the older node is not named as too old: %v", test.language, byNode[old])
		}
		if byNode[current.address]["success"] != true {
			t.Fatalf("language %q: the current node did not apply the limit: %v", test.language, byNode[current.address])
		}
	}
	// Every Agent has the request limits, so the same reply to one of them is
	// not a version problem.
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/rate-limit/put/"+old, f.admin, map[string]any{"qps": 100})
	if byNode := results(body); status != 200 || byNode[old]["code"] != nil || byNode[old]["message"] != "invalid node setting" {
		t.Fatal("a request limit rejection was called a version problem", status, body)
	}
	directories := "节点设置无效：登记目录必须是该节点上的绝对路径，且最多 100 个"
	for _, test := range []struct {
		node   string
		change map[string]any
		want   string
	}{
		// The older node names no setting, but only the directories were sent.
		{old, map[string]any{"advertisedLocalRoots": []string{"relative/data"}}, directories},
		{current.address, map[string]any{"advertisedLocalRoots": []string{"relative/data"}}, directories},
		// 64 objects per child with the default 32 child tasks exceed 1024.
		{current.address, map[string]any{"objectsPerChild": 64}, "节点设置无效：子任务并发须为 1–256，每个子任务的对象并发须为 1–64，两者乘积不能超过 1024"},
		// With more than the directories sent, the older node's reply stays general.
		{old, map[string]any{"advertisedLocalRoots": []string{"relative/data"}, "objectsPerChild": 2}, "节点设置无效"},
	} {
		status, _, body, err := localizedRequest(f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+test.node, f.admin, "zh-CN", test.change)
		if err != nil || status != 400 || body["message"] != test.want {
			t.Fatalf("%s %v: %d %v %v", test.node, test.change, status, body, err)
		}
	}
}

// Every rejection an Agent can name is translated, and the general one does not
// point at the concurrency settings, which it may have nothing to do with.
func TestNodeSettingRejectionsAreLocalized(t *testing.T) {
	for _, err := range []error{nodecontrol.ErrInvalid, nodecontrol.ErrInvalidRoots, nodecontrol.ErrInvalidConcurrency, nodecontrol.ErrUnknownRate} {
		entry, ok := apiMessages[err.Error()]
		if !ok || entry.en == "" || entry.zh == "" || entry.ja == "" || entry.zh == entry.en || entry.ja == entry.en {
			t.Fatalf("%q is not localized: %+v", err, entry)
		}
	}
	if entry := apiMessages[nodecontrol.ErrInvalid.Error()]; strings.Contains(entry.zh, "并发") || strings.Contains(entry.ja, "並列") {
		t.Fatal("the general rejection names the concurrency settings", entry)
	}
}

// Enrolled Agents answer through the real broker: one confirms the rate and one
// rejects it. The console localizes the outcome by its code, so the code and the
// English message must not depend on the request language.
func TestNodeRateUpdateMarksUnconfirmedAgentsWithStableCode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	serve := func(name string, status int, reply any) string {
		address := name + ".example"
		code, session := f.join(t, f.invite(t, address), name, agentCredential(t))
		if code != 200 {
			t.Fatal("join failed", name, code)
		}
		// This helper answers every brokered command for the session, not only scan releases.
		serveScanReleaseAgent(t, f, task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID}, func(command agenttransport.Command) (any, int) {
			if command.Operation != "node_rate_set" {
				return map[string]string{"code": "unknown_operation"}, 404
			}
			var request struct {
				Type string  `json:"type"`
				QPS  float64 `json:"qps"`
			}
			if err := json.Unmarshal(command.Payload, &request); err != nil || request.Type != "put" || request.QPS != 100 {
				t.Errorf("%s received %s: %v", name, command.Payload, err)
			}
			return reply, status
		})
		return address
	}
	confirmed := serve("confirmed", 200, map[string]any{"put": map[string]any{"limitQps": 100}})
	rejected := serve("rejected", 503, map[string]string{"code": "unavailable", "message": "node controller is unavailable"})
	for _, language := range []string{"", "zh-CN", "ja-JP"} {
		status, _, body, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/rate-limit/put", f.admin, language, map[string]any{"qps": 100})
		if err != nil || status != 200 {
			t.Fatalf("language %q: %d %v %v", language, status, body, err)
		}
		if body["success"] != false || body["code"] != "RATE_UPDATE_UNCONFIRMED" || body["message"] != "Some Agents did not confirm the rate update" || body["successCount"] != float64(1) || body["failedCount"] != float64(1) {
			t.Fatalf("language %q: partial update not marked: %v", language, body)
		}
		results := map[string]map[string]any{}
		for _, raw := range body["results"].([]any) {
			result := raw.(map[string]any)
			results[result["workerIp"].(string)] = result
		}
		if results[confirmed]["success"] != true || results[rejected]["success"] != false || results[rejected]["message"] != "node controller is unavailable" || results[rejected]["displayLabel"] == "" {
			t.Fatalf("language %q: per-node results changed: %v", language, results)
		}
	}
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/rate-limit/put/"+confirmed, f.admin, map[string]any{"qps": 100})
	if _, found := body["code"]; status != 200 || body["success"] != true || found || body["message"] != "Node rate updated" {
		t.Fatal("confirmed update reported a failure code", status, body)
	}
}

func TestNodeHTTPAuthorizationAndOfflineNodeRemainVisible(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for _, path := range []string{"/api/master/worker-status", "/api/master/rate-limit/history", "/api/master/rate-limit/adjustment", "/api/master/local-nodes"} {
		if status, _ := request(t, f.http.Client(), "GET", f.http.URL+path, "", nil); status != 401 {
			t.Fatal("node metadata accessible without admin", path, status)
		}
	}
	a := nodeTestAgent(t, f, "offline")
	a.close(t)
	started := time.Now()
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/worker-status", f.admin, nil)
	if status != 200 || body["complete"] != false || body["partial"] != true || time.Since(started) > 7*time.Second {
		t.Fatal("offline node omitted, reported complete, or request unbounded", status, body)
	}
	workers := body["workers"].(map[string]any)
	if len(workers) != 1 || workers[a.address].(map[string]any)["success"] != false {
		t.Fatal("offline registered node disappeared", workers)
	}
}
