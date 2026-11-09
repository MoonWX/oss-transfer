package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// An Agent removed on the Master must stop with a clear log line and an exit
// status its service manager does not restart, instead of looping.
func TestRemovedAgentStopsWithoutRestart(t *testing.T) {
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
	server := httptest.NewServer(handler)
	defer server.Close()
	registry := cluster.New(masterDB, nil)
	invitation, err := registry.Invite("removed-node", "Removed Node")
	if err != nil {
		t.Fatal(err)
	}
	agentDB, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentDB.Close()
	runner, err := agent.New(agentDB, agent.Options{MasterURL: server.URL, JoinToken: invitation.Token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = runner.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Revoke(runner.Session().NodeID); err != nil {
		t.Fatal(err)
	}
	restarted, err := agent.New(agentDB, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = restarted.Run(ctx)
	if !errors.Is(err, agenttransport.ErrRevoked) {
		t.Fatal("removed Agent did not report a revoked enrollment", err)
	}
	stopped := revocation(ctx, err, "oss-transfer-agent", &bytes.Buffer{})
	if !strings.Contains(stopped.Error(), "will not restart automatically") || !strings.Contains(stopped.Error(), "--uninstall --role agent") {
		t.Fatal("service log does not explain the stop", stopped)
	}
	if exitStatus(stopped) != revokedExitStatus || exitStatus(errors.New("other failure")) != 1 {
		t.Fatal("revocation did not select the non-restarting exit status")
	}
}

func TestRevokedContainerAgentIdlesUntilStopped(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "provenance.json")
	if err := os.WriteFile(marker, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := containerMarker
	containerMarker = marker
	t.Cleanup(func() { containerMarker = previous })
	ctx, cancel := context.WithCancel(context.Background())
	var log bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- revocation(ctx, agenttransport.ErrRevoked, "oss-transfer-agent", &log) }()
	select {
	case err := <-done:
		t.Fatal("container Agent exited before it was stopped", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal("stopped container Agent reported a failure", err)
	}
	if !strings.Contains(log.String(), "container stays idle") {
		t.Fatal("container log does not explain the idle Agent", log.String())
	}
	if err := revocation(context.Background(), agenttransport.ErrSession, "x", &log); !errors.Is(err, agenttransport.ErrSession) || errors.Is(err, agenttransport.ErrRevoked) {
		t.Fatal("a transient session failure was treated as revocation", err)
	}
}

func TestSignalSupervisorOnlySignalsRecordedParent(t *testing.T) {
	directory := t.TempDir()
	pidfile := filepath.Join(directory, "agent.supervisor.pid")
	if err := os.WriteFile(pidfile, []byte("4242\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var signalled []int
	kill := func(pid int) error { signalled = append(signalled, pid); return nil }
	if signalSupervisor(pidfile, 4241, kill) || signalSupervisor(filepath.Join(directory, "missing"), 4242, kill) || signalSupervisor(pidfile, 1, kill) {
		t.Fatal("signalled a process that is not this Agent's supervisor")
	}
	link := filepath.Join(directory, "link.pid")
	if err := os.Symlink(pidfile, link); err != nil {
		t.Fatal(err)
	}
	if signalSupervisor(link, 4242, kill) {
		t.Fatal("followed a symlinked pidfile")
	}
	if !signalSupervisor(pidfile, 4242, kill) || len(signalled) != 1 || signalled[0] != 4242 {
		t.Fatal("did not stop the recorded supervisor", signalled)
	}
}
