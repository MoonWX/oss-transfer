package master

import (
	"net/http"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
)

func TestInvitationStatusTracksOnlyItsCommittedEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	invite := f.invite(t, "new-node")
	path := f.http.URL + "/api/master/agents/invitations/" + tokenKey(invite)
	check := func(want string, nodeID string) {
		t.Helper()
		status, body := request(t, f.http.Client(), "GET", path, f.admin, nil)
		if status != http.StatusOK {
			t.Fatal("invitation status", status, body)
		}
		data := body["data"].(map[string]any)
		if data["status"] != want || data["expiresAt"] == nil {
			t.Fatal("incorrect invitation state", data)
		}
		if nodeID != "" && data["nodeId"] != nodeID {
			t.Fatal("wrong enrollment", data)
		}
		for _, secret := range []string{"token", "inviteToken", "credential", "credentialHash"} {
			if _, present := data[secret]; present {
				t.Fatal("invitation status exposed credentials")
			}
		}
	}
	if status, _ := request(t, f.http.Client(), "GET", path, "", nil); status != http.StatusUnauthorized {
		t.Fatal("unauthenticated invitation status", status)
	}
	if status, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/agents/invitations/not-an-id", f.admin, nil); status != http.StatusNotFound {
		t.Fatal("unknown invitation status", status)
	}
	check("pending", "")
	if status, _ := f.join(t, f.invite(t, "existing-node"), "existing-node", agentCredential(t)); status != http.StatusOK {
		t.Fatal(status)
	}
	check("pending", "")
	f.master.options.AgentArtifactBase = "https://downloads.example"
	f.master.options.AgentArtifactVersion = "v4.1.12"
	response, err := f.http.Client().Get(f.http.URL + "/api/agents/bootstrap/" + invite)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("bootstrap", response.StatusCode)
	}
	check("pending", "")
	if status, _ := f.join(t, invite, "new-node", agentCredential(t)); status != http.StatusOK {
		t.Fatal(status)
	}
	check("joined", "new-node")
	// Status survives both restart and the invitation's original deadline.
	f.close()
	f.open(t)
	f.master.agents.registry = cluster.New(f.db, func() time.Time { return time.Now().Add(time.Hour) })
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/login", "", map[string]string{"username": "admin", "password": agentFixturePassword})
	if status != http.StatusOK {
		t.Fatal(status)
	}
	f.admin = body["token"].(string)
	path = f.http.URL + "/api/master/agents/invitations/" + tokenKey(invite)
	check("joined", "new-node")
	if _, err := f.master.agents.registry.Revoke("new-node"); err != nil {
		t.Fatal(err)
	}
	check("revoked", "")
	if status, _ := f.join(t, f.invite(t, "new-node"), "new-node", agentCredential(t)); status != http.StatusOK {
		t.Fatal(status)
	}
	check("revoked", "")
}

func TestInvitationStatusExpiresWithoutClaimingEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	invite := f.invite(t, "unjoined-node")
	f.master.agents.registry = cluster.New(f.db, func() time.Time { return time.Now().Add(time.Hour) })
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/agents/invitations/"+tokenKey(invite), f.admin, nil)
	if status != http.StatusOK || body["data"].(map[string]any)["status"] != "expired" {
		t.Fatal(status, body)
	}
}
