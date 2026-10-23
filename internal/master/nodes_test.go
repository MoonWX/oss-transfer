package master

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agentstate"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type nodeFixture struct {
	master                    *agentHTTPFixture
	path, address, credential string
	db                        *state.Store
	service                   *agentservice.Service
	cancel                    context.CancelFunc
	done                      chan error
}

func nodeTestAgent(t *testing.T, f *agentHTTPFixture, name string) *nodeFixture {
	t.Helper()
	n := &nodeFixture{master: f, path: filepath.Join(t.TempDir(), "agent.db"), address: name + ".example", credential: agentCredential(t)}
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
	n.service, err = agentservice.New(n.db, agentservice.Options{})
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
	status, body = call("GET", "/api/master/worker-status", nil)
	if status != 200 || body["complete"] != true {
		t.Fatal(status, body)
	}
	workers := body["workers"].(map[string]any)
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
