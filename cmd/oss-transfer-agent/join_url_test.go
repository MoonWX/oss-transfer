package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestJoinURLOrigins(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyz0123456789_-"
	for _, origin := range []string{"http://10.1.0.43:1688", "https://master.example", "https://[2001:db8::1]:1685"} {
		t.Run(origin, func(t *testing.T) {
			options, err := withJoinURL(origin+"/api/agents/bootstrap/"+token, agent.Options{Address: "node-a", Label: "Node A"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if options.MasterURL != origin || options.JoinToken != token || options.Address != "node-a" || options.Label != "Node A" {
				t.Fatal("invitation did not preserve the enrollment endpoint and node options")
			}
		})
	}
}

func TestJoinURLRejectsMalformedLinks(t *testing.T) {
	for _, value := range []string{
		"",
		"master.example/api/agents/bootstrap/secret",
		"ftp://master.example/api/agents/bootstrap/secret",
		"http:///api/agents/bootstrap/secret",
		"http://:1685/api/agents/bootstrap/secret",
		"http://user:password@master.example/api/agents/bootstrap/secret",
		"http://master.example/api/agents/bootstrap/secret?x=1",
		"http://master.example/api/agents/bootstrap/secret?",
		"http://master.example/api/agents/bootstrap/secret#fragment",
		"http://master.example/api/agents/bootstrap/secret#",
		"http://master.example/api/agents/bootstrap/",
		"http://master.example/api/agents/bootstrap/secret/",
		"http://master.example/prefix/api/agents/bootstrap/secret",
		"http://master.example/api/agents/bootstrap/../secret",
		"http://master.example/api/agents/bootstrap/a%2Fb",
		"http://master.example/api/agents/bootstrap/sec%72et",
		"http://master.example/api/agents/bootstrap/secret\n",
		"http://master.example:invalid/api/agents/bootstrap/secret",
		"http://master.example:0/api/agents/bootstrap/secret",
		"http://master.example:65536/api/agents/bootstrap/secret",
		"http://master.example:/api/agents/bootstrap/secret",
		"http://[2001:db8::1]:0/api/agents/bootstrap/secret",
		"http://[2001:db8::1]:65536/api/agents/bootstrap/secret",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := withJoinURL(value, agent.Options{}, false); err == nil {
				t.Fatal("accepted an invalid invitation URL")
			} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
				t.Fatal("URL error disclosed credentials")
			}
		})
	}
}

func runAgentCommand(args ...string) error {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	defer func() {
		flag.CommandLine, os.Args = oldFlags, oldArgs
	}()
	flag.CommandLine = flag.NewFlagSet("oss-transfer-agent", flag.ContinueOnError)
	os.Args = append([]string{"oss-transfer-agent"}, args...)
	return run()
}

func TestJoinURLRejectsOtherEnrollmentArguments(t *testing.T) {
	t.Setenv("OSS_TRANSFER_JOIN_TOKEN", "")
	link := "http://127.0.0.1:1/api/agents/bootstrap/secret"
	for _, args := range [][]string{
		{"--master-url", "http://another.example"},
		{"--master-url="},
		{"--join-token", "other"},
		{"--join-token="},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := runAgentCommand(append([]string{"--join-url", link, "--enroll-only"}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "use --join-url without") {
				t.Fatalf("conflicting enrollment arguments were not rejected: %v", err)
			}
		})
	}
	t.Setenv("OSS_TRANSFER_JOIN_TOKEN", "environment-secret")
	err := runAgentCommand("--join-url", link, "--enroll-only")
	if err == nil || !strings.Contains(err.Error(), "use --join-url without") {
		t.Fatalf("conflicting environment credential was not rejected: %v", err)
	}
}

func TestJoinURLCommandEnrollsAndReusesDefaultDirectory(t *testing.T) {
	t.Run("new default", func(t *testing.T) {
		testJoinURLDefaultDirectory(t, false)
	})
	t.Run("legacy generated command", func(t *testing.T) {
		testJoinURLDefaultDirectory(t, true)
	})
}

func testJoinURLDefaultDirectory(t *testing.T, legacy bool) {
	t.Setenv("OSS_TRANSFER_JOIN_TOKEN", "")
	t.Chdir(t.TempDir())
	masterDB, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer masterDB.Close()
	handler, _, err := master.New(masterDB, master.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	var joins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/agents/join" {
			joins.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	registry := cluster.New(masterDB, nil)
	invitation, err := registry.Invite("test-node", "Test Node")
	if err != nil {
		t.Fatal(err)
	}
	link := server.URL + "/api/agents/bootstrap/" + invitation.Token
	initialArgs := []string{"--join-url", link, "--enroll-only"}
	dbPath := filepath.Join("data", "agent", "agent.db")
	if legacy {
		initialArgs = []string{"--master-url", server.URL, "--join-token", invitation.Token, "--data-dir", "./agent-data", "--enroll-only"}
		dbPath = filepath.Join("agent-data", "agent.db")
	}
	if err := runAgentCommand(initialArgs...); err != nil {
		t.Fatal(err)
	}
	members, err := registry.Members()
	if err != nil || len(members) != 1 || members[0].Address != "test-node" {
		t.Fatalf("Agent was not registered using the invitation: members=%d err=%v", len(members), err)
	}
	if joins.Load() != 1 {
		t.Fatalf("expected one real enrollment request, got %d", joins.Load())
	}
	if err := runAgentCommand("--join-url", link, "--enroll-only"); err != nil {
		t.Fatal(err)
	}
	if joins.Load() != 1 {
		t.Fatal("repeating the command attempted to consume the invitation again")
	}
	if legacy {
		if _, err := os.Stat(filepath.Join("data", "agent", "agent.db")); !os.IsNotExist(err) {
			t.Fatal("short command created a new database instead of reusing the legacy identity")
		}
	}
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner, err := agent.New(db, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.Session() != members[0].Session() || joins.Load() != 1 {
		t.Fatal("default-directory enrollment was not retained across restarts")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if legacy {
		newDB, err := state.Open(filepath.Join("data", "agent", "agent.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := newDB.Close(); err != nil {
			t.Fatal(err)
		}
		err = runAgentCommand("--join-url", link, "--enroll-only")
		if err == nil || !strings.Contains(err.Error(), "choose --data-dir") {
			t.Fatalf("ambiguous databases were not rejected: %v", err)
		}
		if err := runAgentCommand("--join-url", link, "--data-dir", "./agent-data", "--enroll-only"); err != nil {
			t.Fatal("explicit legacy directory was not respected:", err)
		}
		if joins.Load() != 1 {
			t.Fatal("legacy identity changed after explicitly choosing its database")
		}
	}
}

func TestJoinURLInvalidPortDoesNotCreateDatabase(t *testing.T) {
	t.Setenv("OSS_TRANSFER_JOIN_TOKEN", "")
	t.Chdir(t.TempDir())
	for _, port := range []string{"0", "65536", ""} {
		err := runAgentCommand("--join-url", "http://127.0.0.1:"+port+"/api/agents/bootstrap/secret", "--enroll-only")
		if err == nil || !strings.Contains(err.Error(), "invalid port") {
			t.Fatalf("invalid port was not rejected before enrollment: %v", err)
		}
		if _, err := os.Stat("data"); !os.IsNotExist(err) {
			t.Fatal("invalid invitation origin created a data directory")
		}
	}
}
