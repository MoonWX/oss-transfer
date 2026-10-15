package master

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// The scanner is a real enrolled but disconnected Agent. The online execution
// member must still receive a durable stop, even though the parent cannot finish.
func TestStopDoesNotWaitForOfflineScannerBeforeStoppingOnlineMember(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	offline := task.Node{ID: "offline-scanner", EnrollmentID: "offline-enrollment", Address: "offline.example"}
	if err := f.master.agents.broker.Activate(nodeSession(offline)); err != nil {
		t.Fatal(err)
	}
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{offline, node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	p, _, err := f.master.tasks.repo.Create(spec, "stop-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	stepDone := make(chan struct{})
	var stepErr error
	go func() {
		stepErr = f.master.tasks.step(ctx, p.ID)
		close(stepDone)
	}()
	defer func() {
		cancel()
		select {
		case <-stepDone:
		case <-time.After(5 * time.Second):
			t.Error("stop reconciliation did not finish after cancellation")
		}
	}()
	// Wait for the online Agent's durable fence, not an assumed RPC latency.
	// The scanner must still be awaiting its stop acknowledgement at that point.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		policy, policyErr := service.Limits.Parent(p.ID)
		if policyErr == nil && policy.Stopped {
			break
		}
		select {
		case <-stepDone:
			t.Fatalf("reconciliation returned before the online Agent was fenced: %v", stepErr)
		case <-ctx.Done():
			t.Fatal("online Agent was not durably fenced", ctx.Err())
		case <-tick.C:
		}
	}
	// Neither offline command has an Agent to consume it. Taking the scanner's
	// still-pending command only now proves the online stop did not wait for the
	// scanner's timeout. Do not acknowledge it; cancellation must leave STOPPING.
	scannerPending := make(chan error, 1)
	go func() {
		for range 2 {
			command, pollErr := f.master.agents.broker.Poll(ctx, nodeSession(offline))
			if pollErr != nil {
				scannerPending <- pollErr
				return
			}
			if command.Operation == "scan_stop" {
				scannerPending <- nil
				return
			}
		}
		scannerPending <- errors.New("scanner stop command is missing")
	}()
	select {
	case pollErr := <-scannerPending:
		if pollErr != nil {
			t.Fatal("scanner stop was not pending when the online Agent was fenced", pollErr)
		}
	case <-stepDone:
		t.Fatalf("scanner stopped blocking before the online Agent was fenced: %v", stepErr)
	case <-ctx.Done():
		t.Fatal("scanner stop command was not queued", ctx.Err())
	}
	cancel()
	<-stepDone
	if stepErr == nil {
		t.Fatal("offline scanner falsely acknowledged")
	}
	job := execution.Job{ParentID: p.ID, ChildID: "1-1", Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: t.TempDir()}, Destination: execution.Endpoint{Kind: "LOCAL", Root: t.TempDir()}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: []execution.Item{{ID: "one", Source: transferstore.Object{Key: "one"}, DestinationKey: "one"}}}
	if _, err = service.Engine.Accept(context.Background(), job); !errors.Is(err, execution.ErrStopped) {
		t.Fatalf("online Agent was not durably fenced: %v", err)
	}
	policy, err := service.Limits.Parent(p.ID)
	if err != nil || !policy.Stopped {
		t.Fatalf("online budget not stopped: %+v %v", policy, err)
	}
	current, _ := f.master.tasks.repo.Get(p.ID)
	if current.Status != "STOPPING" {
		t.Fatal(current.Status)
	}
}

func TestStopReconcilesMoreThanOneLedgerPage(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	p, _, err := f.master.tasks.repo.Create(spec, "stop-test")
	if err != nil {
		t.Fatal(err)
	}
	// Engine.Accept alone doesn't wake the service scheduler. Mark these jobs
	// stopped before allowing reconciliation, so no fixture requires S3 I/O.
	service.Close(context.Background())
	for number := 1; number <= 270; number++ {
		c, err := f.master.tasks.repo.Assign(p.ID, 1, number, []transferstore.Object{{Key: filepath.Base(t.TempDir()), Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.Jobs.Accept(context.Background(), c.Job); err != nil {
			t.Fatal(err)
		}
	}
	// Closing the engine leaves read-only journal RPCs available to the live runner.
	if _, err = f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := f.master.tasks.repo.Get(p.ID)
	if err = f.master.tasks.observeStopped(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	children, err := f.master.tasks.repo.Children(p.ID, 256, 32)
	if err != nil || len(children) != 14 {
		t.Fatal(len(children), err)
	}
	for _, c := range children {
		if c.Record == nil {
			t.Fatalf("child %d was not reconciled", c.Job.Number)
		}
	}
}
