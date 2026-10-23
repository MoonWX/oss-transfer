package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestPermanentDeleteCommandPurgesScanAndExecutionButNotTransferredFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("retained contents"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := discovery.Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: source}}
	if _, err := s.Scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	for {
		current, err := s.Scanner.Status(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.ScanComplete {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	batch, _, err := s.Scanner.Batch(ctx, 1, 1, 0)
	if err != nil || len(batch.Objects) != 1 {
		t.Fatalf("missing actual scan results: %+v %v", batch, err)
	}
	job := execution.Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 1,
		Source: spec.Source, Destination: execution.Endpoint{Kind: "LOCAL", Root: target},
		ConflictPolicy: "SKIP", DiscoveryComplete: true,
		Items: []execution.Item{{ID: "data", Source: batch.Objects[0], DestinationKey: "data"}}}
	if err := s.Limits.ApplyParent(ctx, ratelimit.ParentLimit{ParentID: 1, Generation: 1, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Engine.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Start(ctx, job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Engine.Wait(ctx, job.ChildID); err != nil {
		t.Fatal(err)
	}
	if page, err := s.Report(ctx, ReportRequest{ParentID: 1, Generation: 1, ChildID: "1-1", Limit: 1}); err != nil || len(page.Items) != 1 || page.Items[0].Result.Status != execution.ItemSucceeded {
		t.Fatalf("completed report was not retained: %+v %v", page, err)
	}
	command := agenttransport.Command{Request: agenttransport.Request{Operation: "parent_delete", Mutating: true, Payload: json.RawMessage(`{"parentId":1,"generation":1}`)}}
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := s.Execute(ctx, command)
		if err != nil || reply.Status != 200 {
			t.Fatalf("delete attempt %d: %+v %v", attempt, reply, err)
		}
	}
	if rows, err := s.Jobs.List(ctx, 1, "", 10); err != nil || len(rows) != 0 {
		t.Fatalf("execution ledger retained after permanent delete: %d %v", len(rows), err)
	}
	if _, _, err := s.Scanner.Batch(ctx, 1, 1, 0); !errors.Is(err, discovery.ErrDeleted) {
		t.Fatalf("scan manifest remained readable: %v", err)
	}
	if _, err := s.Engine.Accept(ctx, job); !errors.Is(err, execution.ErrDeleted) {
		t.Fatalf("late assignment revived a deleted task: %v", err)
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.Execute(ctx, agenttransport.Command{Request: agenttransport.Request{Operation: "scan_start", Mutating: true, Payload: payload}})
	if err != nil || reply.Status != 409 {
		t.Fatalf("late scan was not rejected as a conflict: %+v %v", reply, err)
	}
	for _, root := range []string{source, target} {
		data, err := os.ReadFile(filepath.Join(root, "data"))
		if err != nil || string(data) != "retained contents" {
			t.Fatalf("task cleanup affected transferred files: %v", err)
		}
	}
}
