package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestChildPollReturnsNotModifiedUntilTheStoredRecordChanges(t *testing.T) {
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
		return nil, nil, fmt.Errorf("read only")
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	service := &Service{Engine: engine}
	put := func(i int, succeeded int64) {
		t.Helper()
		record := execution.Record{Job: execution.Job{ChildID: fmt.Sprintf("1-%d", i), ParentID: 1, Number: i, Generation: 1}, Attempt: 1, Status: execution.Running, UpdatedAt: time.Unix(int64(1700000000+succeeded), 0).UTC(), Counters: execution.Counters{Total: 4, Succeeded: succeeded}}
		if err := db.Update(func(tx *state.Tx) error {
			return tx.Put("execution_jobs", fmt.Sprintf("%020d/%020d", 1, i), record)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		put(i, 0)
	}
	call := func(operation string, request any) (agenttransport.Response, ChildRecordsResponse) {
		t.Helper()
		payload, _ := json.Marshal(request)
		result, err := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Payload: payload}})
		if err != nil {
			t.Fatal(err)
		}
		var page ChildRecordsResponse
		if result.Status == 200 {
			if err := json.Unmarshal(result.Payload, &page); err != nil {
				t.Fatal(err)
			}
		}
		return result, page
	}
	// children_get stays compatible with older Masters: every row still has a
	// record or Missing; the protocol marker and fingerprints are additive.
	_, page := call("children_get", ChildRecordsRequest{ChildIDs: []string{"1-1", "1-2", "1-3", "1-9"}})
	if page.PollProtocol != ChildPollProtocol || len(page.Children) != 4 || !page.Children[3].Missing || page.Children[3].Fingerprint != "" {
		t.Fatalf("children_get page: %+v", page)
	}
	fingerprints := map[string]string{}
	for _, row := range page.Children[:3] {
		if row.Record == nil || row.NotModified || len(row.Fingerprint) != 64 {
			t.Fatalf("children_get row: %+v", row)
		}
		fingerprints[row.ChildID] = row.Fingerprint
	}
	// Older Agents reject unknown request fields, so the fingerprint list
	// needs its own operation rather than an extra children_get field.
	if result, _ := call("children_get", map[string]any{"childIds": []string{"1-1"}, "children": []any{}}); result.Status != 400 {
		t.Fatal("children_get accepted an unknown field", result.Status)
	}
	put(2, 1)
	_, page = call("children_poll", ChildPollRequest{Children: []ChildPollItem{{ChildID: "1-1", Fingerprint: fingerprints["1-1"]}, {ChildID: "1-2", Fingerprint: fingerprints["1-2"]}, {ChildID: "1-3"}, {ChildID: "1-9", Fingerprint: "stale"}}})
	if len(page.Children) != 4 || page.PollProtocol != ChildPollProtocol {
		t.Fatalf("children_poll page: %+v", page)
	}
	unchanged, changed, unknown, missing := page.Children[0], page.Children[1], page.Children[2], page.Children[3]
	if !unchanged.NotModified || unchanged.Record != nil || unchanged.Fingerprint != fingerprints["1-1"] {
		t.Fatalf("unchanged record was resent: %+v", unchanged)
	}
	if changed.NotModified || changed.Record == nil || changed.Record.Counters.Succeeded != 1 || changed.Fingerprint == fingerprints["1-2"] || len(changed.Fingerprint) != 64 {
		t.Fatalf("changed record not returned: %+v", changed)
	}
	if unknown.NotModified || unknown.Record == nil || unknown.Fingerprint != fingerprints["1-3"] {
		t.Fatalf("record without a known fingerprint not returned: %+v", unknown)
	}
	if !missing.Missing || missing.NotModified || missing.Record != nil {
		t.Fatalf("missing record: %+v", missing)
	}
	if result, _ := call("children_poll", ChildPollRequest{Children: make([]ChildPollItem, 17)}); result.Status != 400 {
		t.Fatal("unbounded poll accepted", result.Status)
	}
	if result, _ := call("children_poll", ChildPollRequest{}); result.Status != 400 {
		t.Fatal("empty poll accepted", result.Status)
	}
}
