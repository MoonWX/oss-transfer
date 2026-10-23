package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type batchLocalFactory struct{}

func (batchLocalFactory) OpenEndpoint(_ context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	return transferstore.NewLocal(endpoint.Root)
}

func TestScanBatchesPreservesOrderAcknowledgementsAndGeneration(t *testing.T) {
	root := t.TempDir()
	for i := range 140 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d", i)), []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scanner, err := discovery.New(db, batchLocalFactory{}, discovery.Options{MaxBatchObjects: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = scanner.Start(ctx, discovery.Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}); err != nil {
		t.Fatal(err)
	}
	for {
		s, err := scanner.Status(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if s.ScanComplete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	service := &Service{Scanner: scanner}
	call := func(request ScanBatchesRequest) agenttransport.Response {
		t.Helper()
		payload, _ := json.Marshal(request)
		response, err := service.Execute(ctx, agenttransport.Command{Request: agenttransport.Request{Operation: "scan_batches", Payload: payload}})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := call(ScanBatchesRequest{ScanRequest: ScanRequest{ParentID: 1, Generation: 1}, Limit: 16})
	if response.Status != 200 {
		t.Fatal(response.Status, string(response.Payload))
	}
	var page ScanBatchesResponse
	if err := json.Unmarshal(response.Payload, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Batches) != 16 {
		t.Fatal("short batch page", len(page.Batches))
	}
	for i, batch := range page.Batches {
		want, found, err := scanner.Batch(ctx, 1, 1, int64(i))
		if err != nil || !found || !reflect.DeepEqual(batch, want) {
			t.Fatalf("batch %d changed: %v", i, err)
		}
	}
	snapshot, err := scanner.Status(ctx, 1)
	if err != nil || snapshot.AckedThrough != 0 {
		t.Fatal("read acknowledged scanner history", snapshot, err)
	}
	if response := call(ScanBatchesRequest{ScanRequest: ScanRequest{ParentID: 1, Generation: 2}, Limit: 16}); response.Status != 409 {
		t.Fatal("stale generation allowed", response.Status)
	}
	if response := call(ScanBatchesRequest{ScanRequest: ScanRequest{ParentID: 1, Generation: 1}, Limit: 17}); response.Status != 400 {
		t.Fatal("unbounded page allowed", response.Status)
	}
	if err := scanner.DeleteParent(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if response := call(ScanBatchesRequest{ScanRequest: ScanRequest{ParentID: 1, Generation: 1}, Limit: 16}); response.Status != 409 {
		t.Fatal("deleted scan readable", response.Status)
	}
}
