package master

import "testing"

// Uses the real Master API and reverse Agent channel, then reopens the Agent DB.
func TestNodeConcurrencyAppliedImmediatelyAndPersists(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a := nodeTestAgent(t, f, "concurrency-a")
	b := nodeTestAgent(t, f, "concurrency-b")
	call := func(body any) (int, map[string]any) {
		return request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+a.address, f.admin, body)
	}
	status, body := call(map[string]any{"maxConcurrentChildren": 12, "objectsPerChild": 6})
	if status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	assertLimits := func(wantChildren, wantObjects int) {
		t.Helper()
		_, children := a.service.Engine.Concurrency()
		if children != wantChildren || a.service.Engine.ObjectConcurrency() != wantObjects {
			t.Fatal("running engine did not apply setting", children, a.service.Engine.ObjectConcurrency())
		}
		_, otherChildren := b.service.Engine.Concurrency()
		if otherChildren != 8 || b.service.Engine.ObjectConcurrency() != 4 {
			t.Fatal("changed another node")
		}
	}
	assertLimits(12, 6)
	status, body = call(map[string]any{"maxConcurrentChildren": 3})
	if status != 200 {
		t.Fatal(status, body)
	}
	assertLimits(3, 6)
	for _, invalid := range []map[string]any{{"maxConcurrentChildren": 0}, {"objectsPerChild": 33}, {"maxConcurrentChildren": 64, "objectsPerChild": 32}, {"maxConcurrentChildren": 64}} {
		if status, _ := call(invalid); status != 400 {
			t.Fatal("invalid setting accepted", invalid, status)
		}
		assertLimits(3, 6)
	}
	if status, _ := call(map[string]any{"label": "Renamed"}); status != 200 {
		t.Fatal(status)
	}
	a.close(t)
	a.open(t)
	assertLimits(3, 6)
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/worker-status", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	load := body["workers"].(map[string]any)[a.address].(map[string]any)["executionStatus"].(map[string]any)
	if load["maxConcurrentChildren"] != float64(3) || load["objectsPerChild"] != float64(6) {
		t.Fatal("status did not report applied settings", load)
	}
	if status, _ := request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/nodes/"+a.address, "", map[string]any{"maxConcurrentChildren": 8}); status != 401 {
		t.Fatal("unauthenticated concurrency change", status)
	}
}
