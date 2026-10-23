package agentservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type updateController struct {
	mu     sync.Mutex
	status softwareupdate.Status
}

func TestAgentUpdateDoesNotWaitForOccupiedTransferSlots(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, Options{Updater: &updateController{}, Execution: execution.Options{MaxConcurrentJobs: 1, WorkersPerJob: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := transferstore.NewLocal(source)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Stat(context.Background(), "data")
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: 1, Generation: 1, Revision: 1, BandwidthBytesPerSecond: 1}); err != nil {
		t.Fatal(err)
	}
	job := execution.Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "SKIP", DiscoveryComplete: true, Items: []execution.Item{{ID: "data", Source: object, DestinationKey: "first"}}}
	if _, err := s.Engine.Accept(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Start(context.Background(), job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	job.ChildID, job.Number, job.Items[0].DestinationKey = "1-2", 2, "second"
	if _, err := s.Engine.Accept(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	// The scheduler must encounter the accepted second child with every slot
	// occupied. Waiting for that slot while holding admission would block Start.
	s.signal()
	time.Sleep(250 * time.Millisecond)
	done := make(chan agenttransport.Response, 1)
	go func() {
		reply, _ := s.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "software_update", Mutating: true, Payload: json.RawMessage(`{"version":"4.1.16"}`)}})
		done <- reply
	}()
	select {
	case reply := <-done:
		if reply.Status != 200 {
			t.Fatal(reply)
		}
	case <-time.After(time.Second):
		s.cancel()
		t.Fatal("upgrade waited behind occupied transfer slots")
	}
}

func (u *updateController) Info() softwareupdate.Info {
	return softwareupdate.Info{Role: "agent", Version: "4.1.15", Supported: true}
}
func (u *updateController) Status() softwareupdate.Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}
func (u *updateController) Check(context.Context) (softwareupdate.Release, error) {
	return softwareupdate.Release{Version: "4.1.16"}, nil
}
func (u *updateController) Start(_ context.Context, version string) (softwareupdate.Status, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = softwareupdate.Status{Version: version, Phase: "QUEUED"}
	return u.status, nil
}

func TestAgentUpdateFencesNewWorkButRetainsReadAndStopCommands(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	updater := &updateController{}
	s, err := New(db, Options{Updater: updater})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	call := func(operation, payload string, mutating bool) agenttransport.Response {
		t.Helper()
		reply, err := s.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Payload: json.RawMessage(payload), Mutating: mutating}})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if r := call("software_update", `{"version":"4.1.16"}`, true); r.Status != 200 {
		t.Fatal(r)
	}
	for _, operation := range []string{"child_accept", "child_recover", "scan_start", "scan_resume", "parent_resume", "comparison_start", "inventory_begin"} {
		if r := call(operation, `{}`, true); r.Status != 409 {
			t.Fatalf("%s started during update: %+v", operation, r)
		}
	}
	if r := call("software_info", `{}`, false); r.Status != 200 {
		t.Fatal("status became unavailable", r)
	}
	if r := call("parent_stop", `{"parentId":1,"generation":1}`, true); r.Status == 409 {
		t.Fatal("stop was blocked by update fence", r)
	}
	updater.mu.Lock()
	updater.status.Phase = "FAILED"
	updater.mu.Unlock()
	if r := call("child_accept", `{}`, true); r.Status == 409 {
		t.Fatal("failed updater left admission fenced", r)
	}
}
