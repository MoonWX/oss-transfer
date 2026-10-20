package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/auth"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type agentHTTPFixture struct {
	path   string
	db     *state.Store
	master *Server
	http   *httptest.Server
	admin  string
}

const agentFixturePassword = "local enrollment test"

var agentFixtureCredential struct {
	once sync.Once
	hash string
	err  error
}

func newAgentHTTPFixture(t *testing.T) *agentHTTPFixture {
	t.Helper()
	f := &agentHTTPFixture{path: filepath.Join(t.TempDir(), "master.db")}
	// Business tests start with an existing administrator. First-login and
	// password recovery tests exercise credential creation separately.
	agentFixtureCredential.once.Do(func() {
		agentFixtureCredential.hash, agentFixtureCredential.err = auth.HashPassword(agentFixturePassword)
	})
	if agentFixtureCredential.err != nil {
		t.Fatal(agentFixtureCredential.err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.close)
	err = f.db.Update(func(tx *state.Tx) error {
		if err := tx.Put("users", "admin", User{
			Username: "admin", DisplayName: "admin", Role: "admin",
			PasswordHash: agentFixtureCredential.hash, Revision: 1,
		}); err != nil {
			return err
		}
		return tx.Put("system", "configuration", Configuration{
			Configured: true, CreatedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db = nil
	if initialPassword := f.open(t); initialPassword != "" {
		t.Fatal("existing administrator unexpectedly replaced")
	}
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/login", "", map[string]string{"username": "admin", "password": agentFixturePassword})
	if status != 200 {
		t.Fatalf("fixture login: HTTP %d", status)
	}
	f.admin = body["token"].(string)
	status, _ = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/network", f.admin, map[string]string{"publicUrl": "https://master.example:1685"})
	if status != 200 {
		t.Fatalf("fixture legacy default endpoint: HTTP %d", status)
	}
	return f
}

func (f *agentHTTPFixture) open(t *testing.T) string {
	t.Helper()
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap string
	f.master, bootstrap, err = New(f.db, Options{DataDirectory: filepath.Dir(f.path)})
	if err != nil {
		t.Fatal(err)
	}
	if f.master.agents == nil {
		t.Fatal("New did not install Agent HTTP routes")
	}
	f.http = httptest.NewServer(f.master)
	return bootstrap
}

func (f *agentHTTPFixture) close() {
	if f.master != nil {
		_ = f.master.Close()
	}
	if f.http != nil {
		f.http.Close()
	}
	if f.db != nil {
		_ = f.db.Close()
	}
	f.master, f.http, f.db = nil, nil, nil
}

func (f *agentHTTPFixture) invite(t *testing.T, address string) string {
	t.Helper()
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{"address": address, "label": "QA node"})
	if status != 200 {
		t.Fatalf("invitation: HTTP %d", status)
	}
	data := body["data"].(map[string]any)
	if data["joinUrl"] != "https://master.example:1685/api/agents/join" || data["nativeAgent"].(map[string]any)["available"] != false {
		t.Fatal("invitation address or native availability is incorrect")
	}
	for _, key := range []string{"dockerCommand", "traditionalCommand"} {
		if command, ok := data[key].(string); ok && command != "" {
			t.Fatal("invitation advertises an unimplemented installer")
		}
	}
	return data["inviteToken"].(string)
}

func agentCredential(t *testing.T) string {
	t.Helper()
	token, err := cluster.Token()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *agentHTTPFixture) join(t *testing.T, invite, identity, credential string) (int, agenttransport.Session) {
	t.Helper()
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/agents/join", "", map[string]string{
		"token": invite, "instanceId": identity, "credential": credential, "address": identity + ".example", "label": "Agent-provided label",
	})
	var session agenttransport.Session
	if status == 200 {
		data := body["data"].(map[string]any)
		if _, found := data["credentialHash"]; found {
			t.Fatal("credential digest exposed by enrollment response")
		}
		session = agenttransport.Session{NodeID: data["id"].(string), EnrollmentID: data["enrollmentId"].(string)}
	}
	return status, session
}

func (f *agentHTTPFixture) agentStatus(t *testing.T, method, path, credential string, body any) int {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, f.http.URL+path, input)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Agent-Key", credential)
	response, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func (f *agentHTTPFixture) nodes(t *testing.T) []any {
	t.Helper()
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes", f.admin, nil)
	if status != 200 {
		t.Fatalf("nodes: HTTP %d", status)
	}
	data := body["data"].(map[string]any)
	if data["port"] != float64(1685) || data["masterWebUrl"] != "https://master.example:1685" {
		t.Fatal("node registry lost configured public address")
	}
	return data["nodes"].([]any)
}

type agentCallResult struct {
	response agenttransport.Response
	err      error
}

func beginAgentCall(t *testing.T, f *agentHTTPFixture, session agenttransport.Session, id string) <-chan agentCallResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	result := make(chan agentCallResult, 1)
	go func() {
		response, err := f.master.agents.broker.Call(ctx, session, agenttransport.Request{RequestID: id, Operation: "status", Payload: json.RawMessage(`{"include":"health"}`)})
		result <- agentCallResult{response, err}
	}()
	return result
}

func finishAgentCall(t *testing.T, result <-chan agentCallResult) agentCallResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("Agent call did not settle")
		return agentCallResult{}
	}
}

func agentClient(t *testing.T, f *agentHTTPFixture, credential string) *agenttransport.HTTPTransport {
	t.Helper()
	client, err := agenttransport.NewHTTPTransport(f.http.URL, credential, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAgentHTTPEnrollmentCommandsRestartAndRevocation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	invite, credential := f.invite(t, "node-a.example"), agentCredential(t)
	status, session := f.join(t, invite, "node-a", credential)
	if status != 200 {
		t.Fatalf("join: HTTP %d", status)
	}
	status, repeated := f.join(t, invite, "node-a", credential)
	if status != 200 || repeated != session {
		t.Fatal("retry after lost join response changed enrollment")
	}
	if status, _ := f.join(t, invite, "imposter", agentCredential(t)); status != 403 {
		t.Fatalf("invitation replay by another identity: HTTP %d", status)
	}
	for _, wrong := range []string{invite, f.admin} {
		if status := f.agentStatus(t, "GET", agenttransport.PollPath, wrong, nil); status != 401 {
			t.Fatalf("non-Agent credential accepted by poll: HTTP %d", status)
		}
	}
	if status, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes", credential, nil); status != 401 {
		t.Fatalf("Agent credential accepted by admin endpoint: HTTP %d", status)
	}
	nodes := f.nodes(t)
	if len(nodes) != 1 {
		t.Fatal("joined Agent missing from registry")
	}
	node := nodes[0].(map[string]any)
	if node["label"] != "QA node" || node["master"] != false || node["agent"] != true || node["online"] != true {
		t.Fatal("incorrect joined Agent metadata")
	}
	client := agentClient(t, f, credential)
	first := beginAgentCall(t, f, session, "health-1")
	command, err := client.Poll(context.Background())
	if err != nil || command.Session != session || command.Operation != "status" {
		t.Fatal("authenticated command poll failed", err)
	}
	result := agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"healthy":true}`)}}
	if err := client.Submit(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if received := finishAgentCall(t, first); received.err != nil || string(received.response.Payload) != `{"healthy":true}` {
		t.Fatal("real HTTP Agent result did not reach caller", received.err)
	}
	f.close()
	if bootstrap := f.open(t); bootstrap != "" {
		t.Fatal("restart reopened initialization")
	}
	if f.master.agents.broker.Stats().Sessions != 1 {
		t.Fatal("persisted enrollment not restored into broker")
	}
	nodes = f.nodes(t)
	if len(nodes) != 1 || nodes[0].(map[string]any)["online"] != false {
		t.Fatal("restart invented an online heartbeat")
	}
	client = agentClient(t, f, credential)
	pending := beginAgentCall(t, f, session, "health-2")
	command, err = client.Poll(context.Background())
	if err != nil {
		t.Fatal("persisted credential cannot poll after restart", err)
	}
	status, _ = request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/node-a.example", f.admin, nil)
	if status != 200 {
		t.Fatalf("revoke: HTTP %d", status)
	}
	if received := finishAgentCall(t, pending); !errors.Is(received.err, agenttransport.ErrSession) {
		t.Fatal("revocation did not terminate in-flight request", received.err)
	}
	if len(f.nodes(t)) != 0 || f.master.agents.broker.Stats().Sessions != 0 {
		t.Fatal("revoked Agent remains active")
	}
	if status := f.agentStatus(t, "GET", agenttransport.PollPath, credential, nil); status != 401 {
		t.Fatalf("revoked credential poll: HTTP %d", status)
	}
	if status := f.agentStatus(t, "POST", agenttransport.ResultPath, credential, result); status != 401 {
		t.Fatalf("revoked credential result: HTTP %d", status)
	}
	if status, _ := f.join(t, invite, "node-a", credential); status != 403 {
		t.Fatalf("old invitation reactivated revoked Agent: HTTP %d", status)
	}
	newInvite := f.invite(t, "node-a.example")
	if status, _ := f.join(t, newInvite, "node-a", credential); status != 409 {
		t.Fatalf("revoked credential reused in new enrollment: HTTP %d", status)
	}
	newCredential := agentCredential(t)
	status, replacement := f.join(t, newInvite, "node-a", newCredential)
	if status != 200 || replacement == session {
		t.Fatal("replacement enrollment failed to replace identity fence")
	}
	if status := f.agentStatus(t, "GET", agenttransport.PollPath, credential, nil); status != 401 {
		t.Fatalf("old credential revived after rejoin: HTTP %d", status)
	}
	f.close()
	_ = f.open(t)
	if status := f.agentStatus(t, "GET", agenttransport.PollPath, credential, nil); status != 401 {
		t.Fatalf("revocation lost after restart: HTTP %d", status)
	}
	if f.master.agents.broker.Stats().Sessions != 1 {
		t.Fatal("restart restored an old or duplicate enrollment")
	}
}

func TestAgentHTTPAdministrativeAuthorization(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/master/agents/invitations"}, {"GET", "/api/master/nodes"}, {"POST", "/api/master/nodes"}, {"PUT", "/api/master/nodes/example"}, {"DELETE", "/api/master/nodes/example"},
	} {
		status, _ := request(t, f.http.Client(), endpoint.method, f.http.URL+endpoint.path, "", map[string]string{})
		if status != 401 {
			t.Fatalf("unprotected admin operation %s %s: HTTP %d", endpoint.method, endpoint.path, status)
		}
	}
	req, _ := http.NewRequest("GET", f.http.URL+"/api/master/nodes", nil)
	req.Header.Set("Authorization", f.admin)
	response, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("missing Bearer scheme accepted")
	}
	if err := f.db.Update(func(tx *state.Tx) error {
		var user User
		if _, err := tx.Get("users", "admin", &user); err != nil {
			return err
		}
		user.Role = "viewer"
		return tx.Put("users", "admin", user)
	}); err != nil {
		t.Fatal(err)
	}
	status, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{})
	if status != 403 {
		t.Fatalf("non-administrator created invitation: HTTP %d", status)
	}
}

func TestAgentHTTPConcurrentJoinRetryCannotUndoRevocation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	invite, credential := f.invite(t, "race.example"), agentCredential(t)
	if status, _ := f.join(t, invite, "race", credential); status != 200 {
		t.Fatalf("join: HTTP %d", status)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body, _ := json.Marshal(map[string]string{"token": invite, "instanceId": "race", "credential": credential})
			response, err := f.http.Client().Post(f.http.URL+"/api/agents/join", "application/json", bytes.NewReader(body))
			if err != nil {
				statuses <- 0
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	close(start)
	status, _ := request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/race.example", f.admin, nil)
	if status != 200 {
		t.Fatalf("revoke: HTTP %d", status)
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != 200 && status != 403 {
			t.Fatalf("join race response: HTTP %d", status)
		}
	}
	if len(f.nodes(t)) != 0 || f.master.agents.broker.Stats().Sessions != 0 {
		t.Fatal("join retry undid a committed revocation")
	}
	if status := f.agentStatus(t, "GET", agenttransport.PollPath, credential, nil); status != 401 {
		t.Fatalf("revoked Agent polling reactivated broker: HTTP %d", status)
	}
}
