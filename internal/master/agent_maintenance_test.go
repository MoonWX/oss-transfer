package master

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func openMaintenanceFixture(t *testing.T, f *agentHTTPFixture, now func() time.Time) {
	t.Helper()
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.master, _, err = New(f.db, Options{DataDirectory: filepath.Dir(f.path), LocalCLIEnabled: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f.http = httptest.NewServer(f.master)
}

func TestAgentMaintenancePersistsAndExpiresWithoutRenewal(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.close()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	openMaintenanceFixture(t, f, now)
	credential := agentCredential(t)
	code, enrolled := f.join(t, f.invite(t, "upgrade-node"), "upgrade-node", credential)
	if code != 200 {
		t.Fatal(code)
	}
	token := localCLIGrant(t, f, credential, 200)
	id := strings.Repeat("a", 64)
	call := func(action, operation, auth string, want int) map[string]any {
		t.Helper()
		code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/maintenance", auth, map[string]string{"action": action, "id": operation})
		if code != want {
			t.Fatalf("maintenance %s status=%d want=%d body=%v", action, code, want, body)
		}
		return body
	}
	call("begin", id, "", 401)
	call("begin", id, f.master.LocalCLIToken(), 403)
	call("begin", id, f.admin, 403)
	call("begin", "invalid", token, 400)
	first := call("begin", id, token, 200)
	clock.Add(int64(10 * time.Minute))
	if replay := call("begin", id, token, 200); replay["expiresAt"] != first["expiresAt"] {
		t.Fatal("replay renewed maintenance")
	}
	call("begin", strings.Repeat("b", 64), token, 409)
	f.close()
	openMaintenanceFixture(t, f, now)
	if active, err := f.master.agentUnderMaintenance(enrolled.NodeID); err != nil || !active {
		t.Fatal("restart lost maintenance", active, err)
	}
	clock.Add(int64(20 * time.Minute))
	if active, err := f.master.agentUnderMaintenance(enrolled.NodeID); err != nil || active {
		t.Fatal("abandoned upgrade never expired", active, err)
	}
	call("begin", id, token, 409)
	second := strings.Repeat("b", 64)
	call("begin", second, token, 200)
	call("end", id, token, 409)
	if active, err := f.master.agentUnderMaintenance(enrolled.NodeID); err != nil || !active {
		t.Fatal("stale installer cleared new window", active, err)
	}
	call("end", second, token, 200)
	call("end", second, token, 200)
	if active, err := f.master.agentUnderMaintenance(enrolled.NodeID); err != nil || active {
		t.Fatal("successful end retained maintenance", active, err)
	}
}

func TestAgentMaintenanceIsBoundToItsEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.close()
	openMaintenanceFixture(t, f, time.Now)
	credential := agentCredential(t)
	code, enrolled := f.join(t, f.invite(t, "old-node"), "maintenance-node", credential)
	if code != 200 {
		t.Fatal(code)
	}
	token := localCLIGrant(t, f, credential, 200)
	code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/maintenance", token, map[string]string{"action": "begin", "id": strings.Repeat("c", 64)})
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/old-node", f.admin, nil)
	if code != 200 {
		t.Fatal(code)
	}
	if active, err := f.master.agentUnderMaintenance(enrolled.NodeID); err != nil || active {
		t.Fatal("revoked enrollment retained protection", active, err)
	}
	code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/maintenance", token, map[string]string{"action": "end", "id": strings.Repeat("c", 64)})
	if code != 401 {
		t.Fatal("revoked token accepted", code)
	}
}

func TestOnlineAgentMaintenanceHasBoundedExpiry(t *testing.T) {
	f := newAgentHTTPFixture(t)
	q := agentUpdateQueue{Items: []agentUpdateItem{{NodeID: "updating", Phase: "UPDATING", StartedAt: time.Now().Add(-time.Minute)}}}
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	if active, err := f.master.agentUnderMaintenance("updating"); err != nil || !active {
		t.Fatal(active, err)
	}
	q.Items[0].StartedAt = time.Now().Add(-31 * time.Minute)
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	if active, err := f.master.agentUnderMaintenance("updating"); err != nil || active {
		t.Fatal("stale online update suppressed failover", active, err)
	}
}
