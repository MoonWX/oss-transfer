package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestChildRecordsBoundsMissingAndCancellation(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		t.Error("read unexpectedly executed a transfer")
		return nil, nil, fmt.Errorf("read only")
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	service := &Service{Engine: engine}
	// Large historical error fields exercise the response-byte bound without
	// allocating object payloads or asking a peer to accept malformed work.
	if err := db.Update(func(tx *state.Tx) error {
		for i := 1; i <= 6; i++ {
			record := execution.Record{Job: execution.Job{ChildID: fmt.Sprintf("1-%d", i), ParentID: 1, Generation: 1}, Error: strings.Repeat("x", 1<<20)}
			if err := tx.Put("execution_jobs", fmt.Sprintf("%020d/%020d", 1, i), record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context, ids []string) agenttransport.Response {
		t.Helper()
		payload, _ := json.Marshal(ChildRecordsRequest{ChildIDs: ids})
		result, err := service.Execute(ctx, agenttransport.Command{Request: agenttransport.Request{Operation: "children_get", Payload: payload}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	response := call(context.Background(), []string{"1-99", "1-1", "1-2", "1-3", "1-4", "1-5", "1-6"})
	var page ChildRecordsResponse
	if response.Status != 200 || json.Unmarshal(response.Payload, &page) != nil {
		t.Fatal(response.Status, string(response.Payload))
	}
	if len(page.Children) != 4 || !page.Children[0].Missing || page.Children[1].Record.Job.ChildID != "1-1" || len(response.Payload) > 4<<20 {
		t.Fatal("payload cap or item identity incorrect", len(page.Children), len(response.Payload))
	}
	if response := call(context.Background(), make([]string, 17)); response.Status != 400 {
		t.Fatal("unbounded request accepted", response.Status)
	}
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put("execution_jobs", fmt.Sprintf("%020d/%020d", 1, 7), execution.Record{Job: execution.Job{ChildID: "1-7", ParentID: 1, Generation: 1}, Error: strings.Repeat("x", 4<<20)})
	}); err != nil {
		t.Fatal(err)
	}
	if response := call(context.Background(), []string{"1-7"}); response.Status != 413 {
		t.Fatal("single oversized record exceeded the batch cap", response.Status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if response := call(ctx, []string{"1-1"}); response.Status != 408 {
		t.Fatal("cancelled read was accepted", response.Status)
	}
}
