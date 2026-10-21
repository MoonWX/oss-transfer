package master

import (
	"context"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestObservationCommitsCollectedPageWhenNextAgentIsUnavailable(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, online := taskAPIAgent(t, f)
	// Leave real child RPCs available but do not start fixture S3 transfers.
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := task.Node{ID: "unavailable", EnrollmentID: "old", Address: "unavailable.example"}
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{online, missing}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	p, _, err := f.master.tasks.repo.Create(spec, "partial-observation-page")
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "first", Size: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 2, []transferstore.Object{{Key: "second", Size: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Node.ID != online.ID || second.Node.ID != missing.ID {
		t.Fatal("fixture did not distribute its children")
	}
	if _, err := service.Jobs.Accept(context.Background(), first.Job); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.observe(context.Background(), p, 64); err == nil {
		t.Fatal("unavailable second Agent was silently ignored")
	}
	children, err := f.master.tasks.repo.Children(p.ID, 0, 64)
	if err != nil || len(children) != 2 {
		t.Fatalf("children: %d %v", len(children), err)
	}
	if children[0].Record == nil || !children[0].Accepted || children[0].Record.Job.ChildID != first.Job.ChildID {
		t.Fatal("collected outcome was lost when the next Agent failed")
	}
	if children[1].Record != nil || children[1].Accepted {
		t.Fatal("unavailable Agent invented an outcome")
	}
}
