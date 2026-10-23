package master

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type legacyPreflightAgent struct{ *agentservice.Service }

func (a legacyPreflightAgent) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	if command.Operation == "preflight_probe" {
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
	return a.Service.Execute(ctx, command)
}

func preflightTestAgent(t *testing.T, f *agentHTTPFixture, number int, legacy ...bool) task.Node {
	t.Helper()
	credential := agentCredential(t)
	address := fmt.Sprintf("preflight-%d.example", number)
	code, session := f.join(t, f.invite(t, address), fmt.Sprintf("preflight-%d", number), credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var executor agenttransport.Executor = service
	if len(legacy) > 0 && legacy[0] {
		executor = legacyPreflightAgent{service}
	}
	runner := agenttransport.NewRunner(agentClient(t, f, credential), executor, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Agent runner did not stop")
		}
		service.Close(context.Background())
		db.Close()
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		members, err := f.master.onlineAgents()
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			if member.ID == session.NodeID {
				return task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Agent did not connect")
	return task.Node{}
}

func TestSharedPreflightRequiresUpgradeOfOlderAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	newNode, oldNode := preflightTestAgent(t, f, 1), preflightTestAgent(t, f, 2, true)
	root := t.TempDir()
	for _, nodes := range [][]task.Node{{oldNode, newNode}, {newNode, oldNode}} {
		row := taskCreateRequest{Direction: "LOCAL_TO_OSS",
			SourceEndpoint:      &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{nodes[0].Address, root}, {nodes[1].Address, root}}},
			DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination"}}
		code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
		encoded, _ := json.Marshal(response)
		if code != 200 || response["data"].(map[string]any)["valid"] != false || !strings.Contains(string(encoded), "Upgrade Agent "+oldNode.Address) || strings.Contains(string(encoded), "cleanup failed") {
			t.Fatalf("old Agent preflight did not give an actionable failure: %d %s", code, encoded)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("mixed-version validation left a probe", err)
		}
	}
}

func TestSharedPreflightUsesRealCrossAgentReadWriteAndAlwaysCleansUp(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := preflightTestAgent(t, f, 1), preflightTestAgent(t, f, 2)
	shared, separate := t.TempDir(), t.TempDir()
	// Identical ordinary files cannot establish that two roots are shared.
	for _, root := range []string{shared, separate} {
		if err := os.WriteFile(filepath.Join(root, "business.bin"), []byte("keep business data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".oss-transfer-shared-probe-user.probe"), []byte("also user data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	clean := func() {
		t.Helper()
		for _, root := range []string{shared, separate} {
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 2 {
				t.Fatalf("preflight left a test file or removed user data: %v %v", entries, err)
			}
			data, err := os.ReadFile(filepath.Join(root, "business.bin"))
			if err != nil || string(data) != "keep business data" {
				t.Fatal("preflight changed business data")
			}
		}
	}
	row := taskCreateRequest{Direction: "LOCAL_TO_OSS",
		SourceEndpoint:      &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{a.Address, shared}, {b.Address, shared}}},
		DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination"}}
	validate := func(row taskCreateRequest, want bool) map[string]any {
		t.Helper()
		code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
		if code != 200 {
			t.Fatal(code, response)
		}
		result := response["data"].(map[string]any)
		if result["valid"] != want {
			t.Fatalf("preflight validity: want %t, response=%v", want, result)
		}
		clean()
		return result
	}
	valid := validate(row, true)
	for _, raw := range valid["nodeBindings"].([]any) {
		if raw.(map[string]any)["code"] != "SHARED_STORAGE_VERIFIED" {
			t.Fatal("preflight did not report real shared-directory verification")
		}
	}
	row.SourceEndpoint.NodeBindings[1].LocalPath = separate
	validate(row, false)
	code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
	if code != 200 || response["data"].(map[string]any)["acceptedCount"] != float64(0) {
		t.Fatal("creation bypassed shared-directory preflight")
	}
	clean()
	// Both names identify the same enrolled Agent and must not count as two nodes.
	row.SourceEndpoint.NodeBindings[1] = taskBinding{a.ID, shared}
	validate(row, false)
	code, response = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
	if code != 200 || response["data"].(map[string]any)["acceptedCount"] != float64(0) {
		t.Fatal("duplicate Agent creation was accepted")
	}
	parents, _, err := f.master.allTaskParents()
	if err != nil || len(parents) != 0 {
		t.Fatal("failed preflight created a task")
	}
	row.SourceEndpoint.NodeBindings = []taskBinding{{a.Address, filepath.Join(shared, "business.bin")}}
	validate(row, true) // A single-file/read-only source does not require writes.
	row = taskCreateRequest{Direction: "OSS_TO_LOCAL",
		SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
		DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{a.Address, shared}, {b.Address, shared}}}}
	validate(row, true)
	row.DestinationEndpoint.NodeBindings[1].LocalPath = separate
	validate(row, false)
	missing := filepath.Join(shared, "missing-target")
	row.DestinationEndpoint.NodeBindings[1].LocalPath = missing
	validate(row, false)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preflight created an unverified destination directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.LocalRoot, b.LocalRoot = shared, shared
	if _, _, err := f.master.proveSharedDirectory(ctx, []task.Node{a, b}, 0); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatal("canceled preflight did not fail")
	}
	clean()
}
