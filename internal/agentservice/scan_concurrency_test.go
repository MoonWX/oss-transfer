package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func openScanConcurrencyService(t *testing.T, db *state.Store, logf func(string, ...any)) *Service {
	t.Helper()
	s, err := New(db, Options{SpoolDirectory: t.TempDir(), Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scanConcurrencyCall(t *testing.T, s *Service, operation string, payload string, mutating bool) (int, map[string]any) {
	t.Helper()
	reply, err := s.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Payload: json.RawMessage(payload), Mutating: mutating}})
	if err != nil {
		t.Fatal(operation, err)
	}
	var body map[string]any
	if err := json.Unmarshal(reply.Payload, &body); err != nil {
		t.Fatal(operation, string(reply.Payload), err)
	}
	return reply.Status, body
}

func assertScanConcurrencyApplied(t *testing.T, s *Service, want int) {
	t.Helper()
	if _, capacity := s.Scanner.Concurrency(); capacity != want {
		t.Fatalf("the scanner admits %d scans, want %d", capacity, want)
	}
	status, config := scanConcurrencyCall(t, s, "node_config", `{}`, false)
	if status != 200 || config["maxConcurrentScans"] != float64(want) {
		t.Fatalf("node_config: %d %v, want maxConcurrentScans %d", status, config, want)
	}
	status, body := scanConcurrencyCall(t, s, "node_status", `{}`, false)
	load, _ := body["executionStatus"].(map[string]any)
	if status != 200 || load["maxConcurrentScans"] != float64(want) || load["scanConcurrencyProtocol"] != float64(ScanConcurrencyProtocol) {
		t.Fatalf("node_status: %d %v", status, load)
	}
}

// node_config_set applies a scan concurrency to the running scanner at once,
// and the Agent starts with it after a restart.
func TestNodeConfigSetAppliesScanConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := openScanConcurrencyService(t, db, nil)
	t.Cleanup(func() {
		if s != nil {
			_ = s.Close(context.Background())
		}
		if db != nil {
			_ = db.Close()
		}
	})
	assertScanConcurrencyApplied(t, s, nodecontrol.DefaultScanConcurrency)
	status, config := scanConcurrencyCall(t, s, "node_config_set", `{"maxConcurrentScans":5}`, true)
	if status != 200 || config["maxConcurrentScans"] != float64(5) {
		t.Fatalf("node_config_set: %d %v", status, config)
	}
	assertScanConcurrencyApplied(t, s, 5)
	if _, saved := s.Nodes.SavedConcurrency(); saved {
		t.Fatal("a scan concurrency change saved the child concurrency")
	}
	for _, invalid := range []string{`{"maxConcurrentScans":0}`, `{"maxConcurrentScans":17}`, `{"maxConcurrentScans":2.5}`, `{"maxConcurrentScans":"4"}`} {
		if status, body := scanConcurrencyCall(t, s, "node_config_set", invalid, true); status != 400 {
			t.Fatalf("%s: %d %v", invalid, status, body)
		}
		assertScanConcurrencyApplied(t, s, 5)
	}
	// A change of the other settings leaves the scan concurrency.
	if status, body := scanConcurrencyCall(t, s, "node_config_set", `{"maxConcurrentChildren":8}`, true); status != 200 {
		t.Fatalf("child concurrency change: %d %v", status, body)
	}
	assertScanConcurrencyApplied(t, s, 5)
	closing := s
	s = nil
	if err := closing.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closingDB := db
	db = nil
	if err := closingDB.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err = state.Open(path); err != nil {
		t.Fatal(err)
	}
	s = openScanConcurrencyService(t, db, nil)
	assertScanConcurrencyApplied(t, s, 5)
}

// An Agent whose saved scan concurrency is out of bounds starts with the
// default and says so in its service log.
func TestInvalidSavedScanConcurrencyStartsTheAgentWithTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := openScanConcurrencyService(t, db, nil)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		var settings map[string]any
		if _, err := tx.Get("node-control", "settings", &settings); err != nil {
			return err
		}
		settings["scanConcurrency"] = 99
		return tx.Put("node-control", "settings", settings)
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	s = openScanConcurrencyService(t, db, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	assertScanConcurrencyApplied(t, s, nodecontrol.DefaultScanConcurrency)
	mu.Lock()
	defer mu.Unlock()
	want := fmt.Sprintf("ignoring invalid saved scan concurrency 99; using %d", nodecontrol.DefaultScanConcurrency)
	found := false
	for _, line := range lines {
		found = found || line == want
	}
	if !found {
		t.Fatalf("the service log lacks %q: %q", want, lines)
	}
}

func TestCapabilitiesReportScanConcurrencyProtocol(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := openScanConcurrencyService(t, db, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if got := s.LeaseCapabilities()["scanConcurrencyProtocol"]; got != ScanConcurrencyProtocol {
		t.Fatalf("capabilities report scanConcurrencyProtocol %v", got)
	}
}
