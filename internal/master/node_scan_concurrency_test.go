package master

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agentstate"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// scanSettingProcess answers "capabilities" as the Agent process does, by merging
// the service's LeaseCapabilities; the service runs everything else.
type scanSettingProcess struct{ *agentservice.Service }

func (a scanSettingProcess) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		raw, err := json.Marshal(a.LeaseCapabilities())
		return agenttransport.Response{Status: 200, Payload: raw}, err
	}
	return a.Service.Execute(ctx, c)
}

// scanSettingTestAgent is nodeTestAgent with the Agent process's capabilities.
func scanSettingTestAgent(t *testing.T, f *agentHTTPFixture, name string) *nodeFixture {
	t.Helper()
	n := &nodeFixture{master: f, path: filepath.Join(t.TempDir(), "agent.db"), address: name + ".example", credential: agentCredential(t)}
	if status, _ := f.join(t, f.invite(t, n.address), name, n.credential); status != 200 {
		t.Fatal("join failed", status)
	}
	openScanSettingProcess(t, n)
	t.Cleanup(func() { n.close(t) })
	return n
}

func openScanSettingProcess(t *testing.T, n *nodeFixture) {
	t.Helper()
	var err error
	if n.db, err = state.Open(n.path); err != nil {
		t.Fatal(err)
	}
	if n.service, err = agentservice.New(n.db, agentservice.Options{}); err != nil {
		t.Fatal(err)
	}
	journal, err := agentstate.NewJournal(n.db, agentstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan error, 1)
	runner := agenttransport.NewRunner(agentClient(t, n.master, n.credential), scanSettingProcess{n.service}, journal, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { n.done <- runner.Run(ctx) }()
}

// scanSettingLimit is the scan concurrency the node's running scanner applies.
func scanSettingLimit(n *nodeFixture) int {
	_, capacity := n.service.Scanner.Concurrency()
	return capacity
}

// A scan concurrency set through the Master reaches the node's running
// scanner at once, leaves the child concurrency unsaved and survives a
// restart of the Agent.
func TestNodeScanConcurrencyAppliedImmediatelyAndPersists(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a := scanSettingTestAgent(t, f, "scans-a")
	b := scanSettingTestAgent(t, f, "scans-b")
	call := func(body any) (int, map[string]any) {
		return request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+a.address, f.admin, body)
	}
	if scanSettingLimit(a) != nodecontrol.DefaultScanConcurrency {
		t.Fatalf("a new node admits %d scans", scanSettingLimit(a))
	}
	if status, body := call(map[string]any{"maxConcurrentScans": 5}); status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	if scanSettingLimit(a) != 5 || a.service.Nodes.Config().MaxConcurrentScans != 5 {
		t.Fatalf("the node runs %d scans and saved %d, want 5", scanSettingLimit(a), a.service.Nodes.Config().MaxConcurrentScans)
	}
	if scanSettingLimit(b) != nodecontrol.DefaultScanConcurrency {
		t.Fatal("the change reached another node")
	}
	if _, saved := a.service.Nodes.SavedConcurrency(); saved {
		t.Fatal("a scan concurrency change saved the child concurrency")
	}
	if status, body := call(map[string]any{"maxConcurrentChildren": 12, "label": "Scans"}); status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	if scanSettingLimit(a) != 5 {
		t.Fatalf("a change without the field reset the scans to %d", scanSettingLimit(a))
	}
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/worker-status", f.admin, nil)
	workers, _ := body["workers"].(map[string]any)
	worker, _ := workers[a.address].(map[string]any)
	load, _ := worker["executionStatus"].(map[string]any)
	if status != 200 || load["maxConcurrentScans"] != float64(5) || load["scanConcurrencyProtocol"] != float64(agentservice.ScanConcurrencyProtocol) {
		t.Fatalf("worker status reports %v", load)
	}
	a.close(t)
	openScanSettingProcess(t, a)
	if scanSettingLimit(a) != 5 {
		t.Fatalf("the restarted Agent admits %d scans, want 5", scanSettingLimit(a))
	}
}

// The Master refuses a scan concurrency outside 1–16 before it sends
// anything to the node, also when the request changes other settings.
func TestNodeUpdateScanConcurrencyValidation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a := scanSettingTestAgent(t, f, "scans-valid")
	call := func(body any) (int, map[string]any) {
		return request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+a.address, f.admin, body)
	}
	for _, invalid := range []map[string]any{
		{"maxConcurrentScans": 0}, {"maxConcurrentScans": 17}, {"maxConcurrentScans": -1},
		{"maxConcurrentScans": 0, "label": "Renamed", "maxConcurrentChildren": 8},
	} {
		status, body := call(invalid)
		if status != 400 || body["message"] != scanConcurrencyRangeMessage {
			t.Fatalf("%v: %d %v", invalid, status, body)
		}
	}
	for _, malformed := range []map[string]any{{"maxConcurrentScans": 2.5}, {"maxConcurrentScans": "4"}} {
		if status, body := call(malformed); status != 400 {
			t.Fatalf("%v: %d %v", malformed, status, body)
		}
	}
	if scanSettingLimit(a) != nodecontrol.DefaultScanConcurrency {
		t.Fatalf("a refused change reached the node: it admits %d scans", scanSettingLimit(a))
	}
	if _, saved := a.service.Nodes.SavedConcurrency(); saved {
		t.Fatal("a refused change saved the child concurrency")
	}
	members, err := f.master.agents.registry.Members()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Address == a.address && member.Label == "Renamed" {
			t.Fatal("a refused change renamed the node")
		}
	}
	for _, message := range []string{scanConcurrencyRangeMessage, scanConcurrencyUnsupportedMessage} {
		entry, ok := apiMessages[message]
		if !ok || entry.en == "" || entry.zh == "" || entry.ja == "" || entry.zh == entry.en || entry.ja == entry.en {
			t.Fatalf("%q is not localized: %+v", message, entry)
		}
	}
}

// scanSettingOlderAgent answers like an Agent released before scan concurrency: it
// advertises no scanConcurrencyProtocol and decodes node settings strictly.
type scanSettingOlderAgent struct {
	mu      sync.Mutex
	changes []json.RawMessage
}

func (a *scanSettingOlderAgent) ReplaySafe(string) bool { return true }

func (a *scanSettingOlderAgent) Execute(_ context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "capabilities":
		return fakeReply(200, map[string]any{"leaseProtocol": 2, "scanProtocol": 2, "storageAddressingProtocol": 1})
	case "node_config_set":
		a.mu.Lock()
		a.changes = append(a.changes, append(json.RawMessage(nil), c.Payload...))
		a.mu.Unlock()
		var change struct {
			AdvertisedLocalRoots  *[]string `json:"advertisedLocalRoots,omitempty"`
			MaxConcurrentChildren *int      `json:"maxConcurrentChildren,omitempty"`
			ObjectsPerChild       *int      `json:"objectsPerChild,omitempty"`
		}
		if err := fakeDecode(c.Payload, &change); err != nil {
			return fakeReply(400, map[string]string{"code": "invalid_request", "message": "invalid node setting"})
		}
		return fakeReply(200, map[string]any{"advertisedLocalRoots": []string{}, "maxConcurrentChildren": 32, "objectsPerChild": 4})
	}
	return fakeUnknownOperation()
}

func (a *scanSettingOlderAgent) received() []json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]json.RawMessage(nil), a.changes...)
}

// A node whose Agent predates scan concurrency gets a 409 and no command:
// sending the field would make it refuse the whole change. Its other
// settings still change without the field.
func TestNodeUpdateScanConcurrencyNeedsCapability(t *testing.T) {
	f := newAgentHTTPFixture(t)
	older := &scanSettingOlderAgent{}
	node := comparisonTestNode(t, f, "scans-older", older)
	call := func(body any) (int, map[string]any) {
		return request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+node.Address, f.admin, body)
	}
	for _, change := range []map[string]any{{"maxConcurrentScans": 4}, {"maxConcurrentScans": 4, "maxConcurrentChildren": 8, "label": "Older"}} {
		status, body := call(change)
		if status != 409 || body["message"] != scanConcurrencyUnsupportedMessage {
			t.Fatalf("%v: %d %v", change, status, body)
		}
	}
	if sent := older.received(); len(sent) != 0 {
		t.Fatalf("the Master sent %d changes to an Agent that cannot take them: %s", len(sent), sent)
	}
	members, err := f.master.agents.registry.Members()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.ID == node.ID && member.Label == "Older" {
			t.Fatal("a refused change renamed the node")
		}
	}
	if status, body := call(map[string]any{"maxConcurrentChildren": 8}); status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	sent := older.received()
	if len(sent) != 1 {
		t.Fatalf("the Master sent %d changes, want 1", len(sent))
	}
	var forwarded map[string]any
	if err := json.Unmarshal(sent[0], &forwarded); err != nil {
		t.Fatal(err)
	}
	if _, present := forwarded["maxConcurrentScans"]; present || forwarded["maxConcurrentChildren"] != float64(8) {
		t.Fatalf("forwarded %s", sent[0])
	}
}
