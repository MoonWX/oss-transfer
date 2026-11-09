package master

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// An Agent that is still old is asked for its capabilities once per delivery
// pass and once per observation pass, not before every child; each new pass
// asks again, so an upgrade is still noticed.
func TestOldAgentIsAskedOncePerPass(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // hang guard only
	defer cancel()
	runtime := f.master.tasks
	var queries atomic.Int64
	runtime.rangedQuery = func(ctx context.Context, node task.Node) (deliveryProtocols, error) {
		queries.Add(1)
		return runtime.deliveryProtocols(ctx, node)
	}
	agent := rangedTestAgent(t, f, "ranged-pass", true)
	spec := task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "LOCAL"}, Nodes: []task.Node{agent.node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	p, _, err := runtime.repo.Create(spec, "pass")
	if err != nil {
		t.Fatal(err)
	}
	var delivered, undelivered []task.Child
	for number := 1; number <= 6; number++ {
		child, err := runtime.repo.Assign(p.ID, p.Generation, number, []transferstore.Object{{Key: "large-" + string(rune('a'+number)), Size: 1 << 30, ETag: "v1"}})
		if err != nil {
			t.Fatal(err)
		}
		if number <= 3 {
			delivered = append(delivered, child)
		} else {
			undelivered = append(undelivered, child)
		}
	}
	for pass := int64(1); pass <= 2; pass++ {
		retryAgentCall(t, ctx, func() error {
			queries.Store(pass - 1)
			return runtime.deliverChildren(ctx, p, delivered)
		})
		if got := queries.Load(); got != pass {
			t.Fatalf("delivery pass %d: %d capability queries in total, want %d", pass, got, pass)
		}
	}
	// The Agent has never seen these children, so observation re-delivers
	// all three in one pass.
	queries.Store(0)
	observations, err := runtime.observeNode(ctx, p, undelivered, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("observation pass asked %d times for three re-deliveries", got)
	}
	for _, child := range undelivered {
		record, err := agent.service.Engine.Get(ctx, child.Job.ChildID)
		if err != nil || record.Job.RangedDownload != "" {
			t.Fatalf("child %s: %q %v", child.Job.ChildID, record.Job.RangedDownload, err)
		}
	}
	_ = observations
}
