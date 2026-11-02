package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type reenrollFixture struct {
	db       *state.Store
	path     string
	registry *cluster.Registry
	server   *httptest.Server
}

func newReenrollFixture(t *testing.T) *reenrollFixture {
	t.Helper()
	masterDirectory := t.TempDir()
	masterDB, err := state.Open(filepath.Join(masterDirectory, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := master.New(masterDB, master.Options{LocalCLIEnabled: true, DataDirectory: masterDirectory})
	if err != nil {
		masterDB.Close()
		t.Fatal(err)
	}
	f := &reenrollFixture{path: filepath.Join(t.TempDir(), "agent.db"), registry: cluster.New(masterDB, nil), server: httptest.NewServer(s)}
	t.Cleanup(func() {
		f.server.Close()
		s.Close()
		masterDB.Close()
		if f.db != nil {
			f.db.Close()
		}
	})
	f.reopen(t)
	return f
}

func (f *reenrollFixture) reopen(t *testing.T) {
	t.Helper()
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *reenrollFixture) invite(t *testing.T) string {
	t.Helper()
	invite, err := f.registry.Invite("rejoin-node", "Rejoin Node")
	if err != nil {
		t.Fatal(err)
	}
	return invite.Token
}

func (f *reenrollFixture) saved(t *testing.T) Identity {
	t.Helper()
	var identity Identity
	if err := f.db.View(func(tx *state.Tx) error {
		found, err := tx.Get("agent-identity", "self", &identity)
		if err == nil && !found {
			return errors.New("missing saved Agent identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return identity
}

func (f *reenrollFixture) enrolled(t *testing.T) (*Agent, string) {
	t.Helper()
	token := f.invite(t)
	a, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, token
}

func (f *reenrollFixture) credentialStatus(t *testing.T, credential string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/agents/cli-access", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Agent-Key", credential)
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestAgentReenrollmentRequiresFreshInvitationAndFencesOldSession(t *testing.T) {
	f := newReenrollFixture(t)
	old, usedToken := f.enrolled(t)
	before := f.saved(t)
	if _, err := f.registry.Revoke(old.Session().NodeID); err != nil {
		t.Fatal(err)
	}
	repeated, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: usedToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := repeated.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("revoked used invitation reported enrollment success", err)
	}
	if f.saved(t) != before {
		t.Fatal("rejected old invitation changed the saved identity")
	}
	freshToken := f.invite(t)
	replacement, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: freshToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := f.saved(t)
	if replacement.Session().NodeID != old.Session().NodeID || replacement.Session().EnrollmentID == old.Session().EnrollmentID || after.Credential == before.Credential {
		t.Fatal("replacement must keep the instance ID and rotate enrollment and credential")
	}
	if f.credentialStatus(t, before.Credential) != http.StatusUnauthorized || f.credentialStatus(t, after.Credential) != http.StatusOK {
		t.Fatal("old credential revived or replacement credential was not accepted")
	}
	if retired, err := f.registry.IsRevoked(old.Session()); err != nil || !retired {
		t.Fatal("old enrollment tombstone was lost", err)
	}
	var calls int
	replacement.options.Executor = agenttransport.ExecutorFunc(func(context.Context, agenttransport.Command) (agenttransport.Response, error) {
		calls++
		return agenttransport.Response{Status: 200}, nil
	})
	executor := runtimeExecutor{agent: replacement}
	if _, err := executor.Execute(context.Background(), agenttransport.Command{Session: old.Session(), Request: agenttransport.Request{Operation: "old-queued-work"}}); !errors.Is(err, agenttransport.ErrSession) || calls != 0 {
		t.Fatal("old queued session reached the replacement executor", err)
	}
	if _, err := executor.Execute(context.Background(), agenttransport.Command{Session: replacement.Session(), Request: agenttransport.Request{Operation: "new-work"}}); err != nil || calls != 1 {
		t.Fatal("replacement executor did not accept its own session", err)
	}
	f.reopen(t)
	restarted, err := New(f.db, Options{})
	if err != nil || restarted.Session() != replacement.Session() || f.saved(t) != after {
		t.Fatal("replacement enrollment did not survive restart", err)
	}
}

func TestAgentRejectedInvitationRestoresHealthyIdentity(t *testing.T) {
	for _, kind := range []string{"invalid", "active-conflict"} {
		t.Run(kind, func(t *testing.T) {
			f := newReenrollFixture(t)
			old, usedToken := f.enrolled(t)
			before := f.saved(t)
			token, status := "invalid-invitation-token", "HTTP 403"
			if kind == "active-conflict" {
				token, status = f.invite(t), "HTTP 409"
			}
			candidate, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: token})
			if err != nil {
				t.Fatal(err)
			}
			if f.saved(t).Credential == before.Credential {
				t.Fatal("pending credential was not durable before Join")
			}
			if err := candidate.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), status) {
				t.Fatal("unexpected rejection", err)
			}
			f.reopen(t)
			if f.saved(t) != before || f.credentialStatus(t, before.Credential) != http.StatusOK {
				t.Fatal("rejected enrollment damaged the original healthy identity")
			}
			repeated, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: usedToken})
			if err != nil || repeated.Session() != old.Session() {
				t.Fatal("healthy repeated invitation rotated the identity", err)
			}
			if err := repeated.Enroll(context.Background()); err != nil || f.saved(t) != before {
				t.Fatal("healthy repeated invitation lost its idempotency", err)
			}
			if kind == "active-conflict" {
				if _, err := f.registry.Revoke(old.Session().NodeID); err != nil {
					t.Fatal(err)
				}
				replacement, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: token})
				if err != nil {
					t.Fatal(err)
				}
				if err := replacement.Enroll(context.Background()); err != nil || replacement.Session() == old.Session() {
					t.Fatal("conflicting invitation was consumed before a valid re-enrollment", err)
				}
			}
		})
	}
}

func TestAgentRejectedFirstInvitationAllowsAnotherInvitation(t *testing.T) {
	f := newReenrollFixture(t)
	a, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: "invalid-invitation-token"})
	if err != nil {
		t.Fatal(err)
	}
	id := a.Session().NodeID
	if err := a.Enroll(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("invalid first invitation was not rejected", err)
	}
	if f.saved(t).PendingToken != "" || f.saved(t).Credential != "" {
		t.Fatal("definitively rejected first enrollment retained unusable pending credentials")
	}
	if _, err := New(f.db, Options{}); err == nil {
		t.Fatal("rejected first enrollment resumed without an invitation")
	}
	a, err = New(f.db, Options{MasterURL: f.server.URL, JoinToken: f.invite(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err != nil || a.Session().NodeID != id {
		t.Fatal("a fresh valid invitation could not replace the rejected first one", err)
	}
}

type reenrollResponseLoss struct{ lost atomic.Bool }

func (t *reenrollResponseLoss) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && request.URL.Path == "/api/agents/join" && t.lost.CompareAndSwap(false, true) {
		response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

func TestAgentLostReenrollmentResponseRetainsPendingCredential(t *testing.T) {
	f := newReenrollFixture(t)
	old, _ := f.enrolled(t)
	if _, err := f.registry.Revoke(old.Session().NodeID); err != nil {
		t.Fatal(err)
	}
	token := f.invite(t)
	a, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: token, HTTPTransport: &reenrollResponseLoss{}})
	if err != nil {
		t.Fatal(err)
	}
	pending := f.saved(t)
	if err := a.Enroll(context.Background()); err == nil {
		t.Fatal("lost response injection did not run")
	}
	if f.saved(t) != pending {
		t.Fatal("uncertain response discarded the persisted candidate credential")
	}
	f.reopen(t)
	if _, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: f.invite(t)}); err == nil || !strings.Contains(err.Error(), "awaiting confirmation") {
		t.Fatal("another invitation overwrote an unconfirmed enrollment", err)
	}
	if f.saved(t) != pending {
		t.Fatal("new invitation changed the unconfirmed identity")
	}
	a, err = New(f.db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal("persisted enrollment could not reconcile the lost response", err)
	}
	if f.saved(t).Credential != pending.Credential || a.Session().EnrollmentID == "" || a.Session() == old.Session() {
		t.Fatal("response reconciliation generated a different credential or enrollment")
	}
	if f.credentialStatus(t, pending.Credential) != http.StatusOK {
		t.Fatal("reconciled credential was not accepted")
	}
}

type uncertainEnrollmentTransport struct{ status int }

func (t uncertainEnrollmentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.status == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return &http.Response{StatusCode: t.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unconfirmed response")), Request: request}, nil
}

func TestAgentUncertainEnrollmentRetainsCredentialForRetry(t *testing.T) {
	for _, failure := range []struct {
		name   string
		status int
	}{{"connection", 0}, {"unavailable", http.StatusServiceUnavailable}, {"unauthorized-proxy", http.StatusUnauthorized}, {"invalid-response", http.StatusOK}} {
		t.Run(failure.name, func(t *testing.T) {
			f := newReenrollFixture(t)
			old, _ := f.enrolled(t)
			if _, err := f.registry.Revoke(old.Session().NodeID); err != nil {
				t.Fatal(err)
			}
			token := f.invite(t)
			a, err := New(f.db, Options{MasterURL: f.server.URL, JoinToken: token, HTTPTransport: uncertainEnrollmentTransport{status: failure.status}})
			if err != nil {
				t.Fatal(err)
			}
			pending := f.saved(t)
			if err := a.Enroll(context.Background()); err == nil || f.saved(t) != pending {
				t.Fatal("uncertain response lost the pending credential", err)
			}
			f.reopen(t)
			a, err = New(f.db, Options{MasterURL: f.server.URL, JoinToken: token})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Enroll(context.Background()); err != nil || f.saved(t).Credential != pending.Credential {
				t.Fatal("retry replaced the pending credential instead of confirming it", err)
			}
		})
	}
}

func TestAgentReenrollmentDoesNotRetargetAnotherMaster(t *testing.T) {
	f := newReenrollFixture(t)
	f.enrolled(t)
	before := f.saved(t)
	other := newReenrollFixture(t)
	if _, err := New(f.db, Options{MasterURL: other.server.URL, JoinToken: other.invite(t)}); err == nil || !strings.Contains(err.Error(), "another Master") {
		t.Fatal("existing execution state was silently retargeted to another Master", err)
	}
	if f.saved(t) != before {
		t.Fatal("cross-Master rejection changed the original identity")
	}
	if members, err := other.registry.Members(); err != nil || len(members) != 0 {
		t.Fatal("cross-Master rejection consumed the other invitation", err)
	}
}
