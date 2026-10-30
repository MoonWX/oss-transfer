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
	if _, err := f.master.agents.registry.UpdateLabel(oldNode.ID, "Legacy worker"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, nodes := range [][]task.Node{{oldNode, newNode}, {newNode, oldNode}} {
		row := taskCreateRequest{Direction: "LOCAL_TO_OSS",
			SourceEndpoint:      &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{nodes[0].Address, root}, {nodes[1].Address, root}}},
			DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination"}}
		code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
		encoded, _ := json.Marshal(response)
		if code != 200 || response["data"].(map[string]any)["valid"] != false || !strings.Contains(string(encoded), "Upgrade Agent Legacy worker (127.0.0.1)") || strings.Contains(string(encoded), "cleanup failed") {
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

func TestSharedPreflightMissingDestinationsNeverAppearValid(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := preflightTestAgent(t, f, 1), preflightTestAgent(t, f, 2)
	root := t.TempDir()
	missing := filepath.Join(root, "missing-target")
	for _, test := range []struct {
		name     string
		bindings []taskBinding
	}{
		{"both missing", []taskBinding{{a.Address, missing}, {b.Address, missing}}},
		{"first missing", []taskBinding{{a.Address, missing}, {b.Address, root}}},
		{"second missing", []taskBinding{{a.Address, root}, {b.Address, missing}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := taskCreateRequest{Direction: "OSS_TO_LOCAL",
				SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
				DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: test.bindings}}
			code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
			if code != 200 {
				t.Fatal(code, response)
			}
			data := response["data"].(map[string]any)
			checks := data["nodeBindings"].([]any)
			if data["valid"] != false || len(checks) != len(test.bindings) {
				t.Fatal("multi-node validation did not fail all bindings", data)
			}
			for i, raw := range checks {
				check := raw.(map[string]any)
				if check["valid"] != false {
					t.Fatalf("binding %d passed without shared-directory proof: %v", i, check)
				}
				wantCode, wantType := "SHARED_STORAGE_NOT_VERIFIED", "DIRECTORY"
				if test.bindings[i].LocalPath == missing {
					wantCode, wantType = "SHARED_DIRECTORY_REQUIRED", "MISSING"
				}
				if check["code"] != wantCode || check["pathType"] != wantType || check["message"] == "" {
					t.Fatal("incorrect blocked binding result", check)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed preflight created a destination or marker", entries, err)
			}
		})
	}
	// A single download node can still create its destination when the task
	// starts; this read-only preflight must not create it prematurely.
	row := taskCreateRequest{Direction: "OSS_TO_LOCAL",
		SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
		DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{a.Address, missing}}}}
	code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
	if code != 200 || response["data"].(map[string]any)["valid"] != true {
		t.Fatal("single-node missing destination was rejected", code, response)
	}
	checks := response["data"].(map[string]any)["nodeBindings"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["pathType"] != "MISSING" {
		t.Fatal("single-node missing destination lost its path type", checks)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preflight created a destination", err)
	}
	// A failed basic check prevents the shared proof too. Do not overwrite the
	// offline node's actionable reason or leave its online peer marked valid.
	row.DestinationEndpoint.NodeBindings = []taskBinding{{a.Address, root}, {"offline.example", root}}
	code, response = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
	if code != 200 || response["data"].(map[string]any)["valid"] != false {
		t.Fatal("offline node did not fail preflight", code, response)
	}
	checks = response["data"].(map[string]any)["nodeBindings"].([]any)
	if len(checks) != 2 {
		t.Fatal("missing binding check", checks)
	}
	online, offline := checks[0].(map[string]any), checks[1].(map[string]any)
	if online["valid"] != false || online["code"] != "SHARED_STORAGE_NOT_VERIFIED" || online["pathType"] != "DIRECTORY" {
		t.Fatal("online peer passed without shared-directory proof", online)
	}
	if offline["valid"] != false || offline["message"] != "执行节点未连接" {
		t.Fatal("offline node's original failure was lost", offline)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("blocked preflight changed the target directory", entries, err)
	}
}

func TestSharedPreflightCreationRechecksChangedDestination(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := preflightTestAgent(t, f, 1), preflightTestAgent(t, f, 2)
	for _, replacement := range []string{"missing", "file"} {
		t.Run(replacement, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "shared")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			row := taskCreateRequest{Direction: "OSS_TO_LOCAL",
				SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
				DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{a.Address, root}, {b.Address, root}}}}
			code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
			if code != 200 || response["data"].(map[string]any)["valid"] != true {
				t.Fatal("initial shared-directory verification failed", code, response)
			}
			if err := os.Remove(root); err != nil {
				t.Fatal("successful preflight left a marker", err)
			}
			if replacement == "file" {
				if err := os.WriteFile(root, []byte("keep this file"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, response = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
			if code != 200 {
				t.Fatal(code, response)
			}
			data := response["data"].(map[string]any)
			if data["acceptedCount"] != float64(0) || data["rejectedCount"] != float64(1) {
				t.Fatal("creation reused the previous preflight result", data)
			}
			parents, _, err := f.master.allTaskParents()
			if err != nil || len(parents) != 0 {
				t.Fatal("failed creation persisted a task", parents, err)
			}
			if replacement == "missing" {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatal("creation recreated an unverified shared destination", err)
				}
			} else if content, err := os.ReadFile(root); err != nil || string(content) != "keep this file" {
				t.Fatal("creation changed the replacement file", string(content), err)
			}
		})
	}
}
