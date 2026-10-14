package agentservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestReportReadsRealTransferResultsAndPagesStoppedLedger(t *testing.T) {
	ctx := context.Background()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	source, target := t.TempDir(), t.TempDir()
	for _, key := range []string{"=formula.txt", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(source, key), []byte("source bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "skip.txt"), []byte("retained target"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := transferstore.NewLocal(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job := execution.Job{ChildID: "7-1", ParentID: 7, Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "SKIP", DiscoveryComplete: true}
	for _, key := range []string{"=formula.txt", "skip.txt", "missing.txt"} {
		object, err := store.Stat(ctx, key)
		if key == "missing.txt" {
			object = transferstore.Object{Key: key, Size: 12}
		} else if err != nil {
			t.Fatal(err)
		}
		job.Items = append(job.Items, execution.Item{ID: key, Source: object, DestinationKey: key})
	}
	if err := s.Limits.ApplyParent(ctx, ratelimit.ParentLimit{ParentID: 7, Generation: 1, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Engine.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, ReportRequest{ParentID: 7, Generation: 1, ChildID: "7-1", Limit: 1}); !errors.Is(err, execution.ErrRunning) {
		t.Fatalf("unstarted report accepted: %v", err)
	}
	if err := s.Engine.Start(ctx, "7-1", 1); err != nil {
		t.Fatal(err)
	}
	record, err := s.Engine.Wait(ctx, "7-1")
	if err != nil || record.Status != execution.PartialFailed {
		t.Fatal(record.Status, err)
	}
	statuses := []execution.ItemStatus{execution.ItemSucceeded, execution.ItemSkipped, execution.ItemFailed}
	for index, want := range statuses {
		page, err := s.Report(ctx, ReportRequest{ParentID: 7, Generation: 1, ChildID: "7-1", Offset: index, Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].Result.Status != want || page.Total != 3 || page.NextOffset != index+1 || page.Done != (index == 2) || page.JobDigest != ReportJobDigest(job) {
			t.Fatalf("page %d: %+v %v", index, page, err)
		}
		if want == execution.ItemFailed && page.Items[0].Result.Error == "" {
			t.Fatal("failure report lost the actual cause")
		}
	}
	data, err := os.ReadFile(filepath.Join(target, "=formula.txt"))
	if err != nil || string(data) != "source bytes" {
		t.Fatal("report success did not correspond to copied bytes", err)
	}
	data, err = os.ReadFile(filepath.Join(target, "skip.txt"))
	if err != nil || string(data) != "retained target" {
		t.Fatal("skip overwrote destination", err)
	}
	if _, err := s.Report(ctx, ReportRequest{ParentID: 7, Generation: 2, ChildID: "7-1", Limit: 1}); !errors.Is(err, execution.ErrGeneration) {
		t.Fatal("old generation returned", err)
	}
	if _, err := s.Report(ctx, ReportRequest{ParentID: 7, Generation: 1, ChildID: "7-1", Limit: 129}); err == nil {
		t.Fatal("unbounded report page accepted")
	}
}
