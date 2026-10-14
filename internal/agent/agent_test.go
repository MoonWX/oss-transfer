package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type loseJoinResponse struct{ lost atomic.Bool }

func (t *loseJoinResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && request.URL.Path == "/api/agents/join" && t.lost.CompareAndSwap(false, true) {
		response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

func TestAgentIdentitySurvivesLostJoinResponseAndRestart(t *testing.T) {
	masterDB, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer masterDB.Close()
	s, setupToken, err := master.New(masterDB, master.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(s)
	defer server.Close()
	setup, _ := json.Marshal(map[string]string{"username": "admin", "password": "test password", "publicUrl": server.URL, "setupToken": setupToken})
	response, err := http.Post(server.URL+"/api/setup/complete", "application/json", bytes.NewReader(setup))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	registry := cluster.New(masterDB, nil)
	invite, err := registry.Invite("agent-a", "Agent A")
	if err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(db, agent.Options{MasterURL: server.URL, JoinToken: invite.Token, HTTPTransport: &loseJoinResponse{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err == nil {
		t.Fatal("response-loss injection did not run")
	}
	members, err := registry.Members()
	if err != nil || len(members) != 1 {
		t.Fatal("enrollment not committed before lost response", members, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err = agent.New(db, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Session() != members[0].Session() {
		t.Fatal("restart generated another Agent identity")
	}
	// Reusing the same installer arguments must not rotate the enrolled secret.
	b, err := agent.New(db, agent.Options{MasterURL: server.URL, JoinToken: invite.Token})
	if err != nil {
		t.Fatal(err)
	}
	if b.Session() != a.Session() {
		t.Fatal("repeated installation changed enrollment")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Agent shutdown left a poll running")
	}
}

func TestAgentRejectsMasterDatabaseAndUnknownIdentity(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.New(db, agent.Options{}); err == nil {
		t.Fatal("Agent accepted Master database")
	}
}
