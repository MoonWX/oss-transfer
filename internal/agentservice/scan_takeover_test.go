package agentservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestScanTakeoverCommandsRequireMutationAndFenceEpoch(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scanner, err := discovery.New(db, batchLocalFactory{}, discovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	service := &Service{Scanner: scanner}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "new"), []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call := func(operation string, mutating bool, request any) agenttransport.Response {
		t.Helper()
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.Execute(ctx, agenttransport.Command{Request: agenttransport.Request{Operation: operation, Mutating: mutating, Payload: data}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	request := discovery.TakeoverRequest{Spec: discovery.Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}, Epoch: 2}
	if result := call("scan_takeover_seed", false, request); result.Status != 400 {
		t.Fatal(result)
	}
	if result := call("scan_takeover_seed", true, map[string]any{"unknown": true}); result.Status != 400 {
		t.Fatal(result)
	}
	if result := call("scan_takeover_seed", true, request); result.Status != 200 {
		t.Fatal(result.Status, string(result.Payload))
	}
	control := ScanRequest{ParentID: 1, Generation: 1, Epoch: 2}
	if result := call("scan_takeover_finalize", true, control); result.Status != 200 {
		t.Fatal(result.Status, string(result.Payload))
	}
	for {
		current, err := scanner.Status(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.ScanComplete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	result := call("scan_batches", false, ScanBatchesRequest{ScanRequest: control, Limit: 16})
	var page ScanBatchesResponse
	if err := json.Unmarshal(result.Payload, &page); err != nil {
		t.Fatal(err)
	}
	if result.Status != 200 || len(page.Batches) != 1 || page.Batches[0].Epoch != 2 {
		t.Fatal(result.Status, string(result.Payload))
	}
	control.Epoch = 0
	for _, operation := range []string{"scan_status", "scan_ack", "scan_stop", "scan_resume", "scan_takeover_finalize"} {
		if result := call(operation, true, control); result.Status != 409 {
			t.Fatal("stale epoch accepted", operation, result.Status, string(result.Payload))
		}
	}
	if result := call("scan_batches", false, ScanBatchesRequest{ScanRequest: control, Limit: 16}); result.Status != 409 {
		t.Fatal(result)
	}
	oversized := map[string]any{"padding": strings.Repeat("x", discovery.MaxTakeoverPayloadBytes)}
	if result := call("scan_takeover_seed", true, oversized); result.Status != 413 {
		t.Fatal(result)
	}
}
