package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestParentCleanupSurvivesCallerCancellationAndAgentRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()); _ = db.Close() })
	const count = 2051
	if err := db.Update(func(tx *state.Tx) error {
		for _, id := range []int64{1, 11} {
			if err := tx.Put("execution_parents", fmt.Sprint(id), execution.ParentFence{ParentID: id, Generation: 1}); err != nil {
				return err
			}
			if err := tx.Put("discovery-states", fmt.Sprintf("%020d", id), discovery.State{Spec: discovery.Spec{ParentID: id, Generation: 1}, Generation: 1, Status: discovery.Stopped}); err != nil {
				return err
			}
			n := count
			if id == 11 {
				n = 1
			}
			for i := 1; i <= n; i++ {
				key := fmt.Sprintf("%020d/%020d", id, i)
				record := execution.Record{Job: execution.Job{ParentID: id, Generation: 1, ChildID: fmt.Sprintf("%d-%d", id, i), Number: i}, Status: execution.Completed}
				if err := tx.Put("execution_jobs", key, record); err != nil {
					return err
				}
				if err := tx.Put("discovery-objects", key, map[string]any{"source": "kept until permanent delete"}); err != nil {
					return err
				}
				if err := tx.Put("discovery-batches", key, discovery.Batch{ParentID: id, Generation: 1, Number: int64(i)}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "transferred-file")
	if err := os.WriteFile(marker, []byte("remain on disk"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := agenttransport.Command{Request: agenttransport.Request{Operation: "parent_cleanup", Mutating: true, Payload: json.RawMessage(`{"parentId":1,"generation":1}`)}}
	reply, err := service.Execute(ctx, cmd)
	cancel()
	if err != nil || reply.Status != 200 {
		t.Fatalf("admission: %+v %v", reply, err)
	}
	var admitted ParentCleanupProgress
	if err := json.Unmarshal(reply.Payload, &admitted); err != nil || admitted.Done {
		t.Fatalf("expected background progress: %+v %v", admitted, err)
	}
	if !service.ReplaySafe("parent_cleanup") {
		t.Fatal("progress polls would return journal-cached responses")
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service, err = New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Only read progress after restart. No second cleanup request drives workers.
	deadline := time.Now().Add(10 * time.Second)
	var complete execution.DeletionProgress
	var scan discovery.State
	for {
		complete, err = service.Engine.DeletionProgress(context.Background(), 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		scan, err = service.Scanner.Status(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if complete.Done && scan.Purged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not resume without RPC: %+v %+v", complete, scan)
		}
		time.Sleep(time.Millisecond)
	}
	if complete.RecordsDeleted+scan.CleanupDeleted != count*3 {
		t.Fatalf("lost durable progress: execution=%d scan=%d", complete.RecordsDeleted, scan.CleanupDeleted)
	}
	reply, err = service.Execute(context.Background(), cmd)
	var replay ParentCleanupProgress
	if err != nil || reply.Status != 200 || json.Unmarshal(reply.Payload, &replay) != nil || !replay.Done || replay.RecordsDeleted != count*3 {
		t.Fatalf("replay reset progress: %+v %v", reply, err)
	}
	if _, err := service.Jobs.Get(context.Background(), "11-1"); err != nil {
		t.Fatalf("other parent's history removed: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "remain on disk" {
		t.Fatalf("cleanup affected transferred file: %v", err)
	}
	if _, err := service.Jobs.Get(context.Background(), "1-1"); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("execution retained: %v", err)
	}
}

func TestParentCleanupRejectsStaleDiscoveryBeforeFencingExecution(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put("discovery-states", "00000000000000000001", discovery.State{Spec: discovery.Spec{ParentID: 1, Generation: 2}, Generation: 2, Status: discovery.Stopped})
	}); err != nil {
		t.Fatal(err)
	}
	cmd := agenttransport.Command{Request: agenttransport.Request{Operation: "parent_cleanup", Mutating: true, Payload: json.RawMessage(`{"parentId":1,"generation":1}`)}}
	reply, err := service.Execute(context.Background(), cmd)
	if err != nil || reply.Status != 409 {
		t.Fatalf("stale request accepted: %+v %v", reply, err)
	}
	if _, err := service.Jobs.Parent(context.Background(), 1); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("stale request changed execution fence: %v", err)
	}
	cmd.Mutating = false
	if reply, err := service.Execute(context.Background(), cmd); err != nil || reply.Status != 400 {
		t.Fatalf("cleanup accepted a read-only command: %+v %v", reply, err)
	}
	cmd.Mutating = true
	cmd.Payload = json.RawMessage(`{"parentId":1,"generation":3}`)
	if reply, err := service.Execute(context.Background(), cmd); err != nil || reply.Status != 200 {
		t.Fatalf("newer owning generation could not retire old Agent state: %+v %v", reply, err)
	}
}
