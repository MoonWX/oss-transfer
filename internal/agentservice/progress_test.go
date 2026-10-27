package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestTaskProgressRPCReportsSupportAndDoesNotReadLedger(t *testing.T) {
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
		return nil, nil, errors.New("progress must not open storage")
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	service := &Service{Engine: engine, Rates: NewTaskRates()}
	// Both observations must work while the ledger is unavailable. They must
	// neither read historical jobs nor create new persistent progress records.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	call := func(operation, payload string) agenttransport.Response {
		t.Helper()
		value, err := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Payload: json.RawMessage(payload)}})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	result := call("task_metrics", `{"parentId":1,"generation":1}`)
	var metrics TaskRateSnapshot
	if result.Status != 200 || json.Unmarshal(result.Payload, &metrics) != nil || !metrics.ProgressSupported || metrics.ParentID != 1 || metrics.Generation != 1 || len(metrics.Progress) != 0 {
		t.Fatalf("metrics capability response: %d %s", result.Status, result.Payload)
	}
	result = call("task_progress", `{"parentId":1,"generation":1,"childId":"1-1"}`)
	var details struct {
		Supported bool                       `json:"supported"`
		Child     *execution.ChildProgress   `json:"child"`
		Items     []execution.ObjectProgress `json:"items"`
	}
	if result.Status != 200 || json.Unmarshal(result.Payload, &details) != nil || !details.Supported || details.Child != nil || details.Items == nil || len(details.Items) != 0 {
		t.Fatalf("detail capability response: %d %s", result.Status, result.Payload)
	}
	for _, payload := range []string{`{}`, `{"parentId":1,"generation":1}`, `{"parentId":0,"generation":1,"childId":"1-1"}`, `{"parentId":1,"generation":0,"childId":"1-1"}`, `{"parentId":1,"generation":1,"childId":"1-1","extra":true}`} {
		if result := call("task_progress", payload); result.Status != 400 {
			t.Fatalf("invalid progress request accepted: %s %d", payload, result.Status)
		}
	}
}
