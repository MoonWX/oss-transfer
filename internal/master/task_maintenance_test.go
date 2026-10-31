package master

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Exercise actual outbound Agent channels and lease cancellation. An upgrade
// must preserve scanner ownership beyond lease expiry without extending the
// stopped Agent's permission to write. A lost installer must eventually allow
// the normal failover path again.
func TestPlannedAgentDowntimePreservesScanUntilMaintenanceExpires(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "maintenance-a"), takeoverTestAgent(t, f, "maintenance-b")
	p := takeoverParent(t, f, a.node, b.node)
	_, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "saved", Size: 3}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := f.master.tasks
	if err := runtime.repo.ScanProgress(p.ID, p.Generation, 5000, 30000, 40, false, ""); err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the production lease duration while the real HTTP/DB handshake
	// completes. Advance only the Master's fence once the Agent has stopped.
	clockStart := time.Now()
	var elapsed atomic.Int64
	runtime.leases.now = func() time.Time { return clockStart.Add(time.Duration(elapsed.Load())) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	startTakeoverLeaseLoop(t, ctx, runtime)
	p, err = runtime.reconcileFailover(ctx, p, map[string]bool{a.node.ID: true, b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	before := p
	lease, err := a.service.Leases.ParentContext(p.ID, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	reservation := agentMaintenance{ID: "installer-operation", EnrollmentID: a.node.EnrollmentID, ExpiresAt: f.master.options.Now().Add(30 * time.Minute)}
	save := func() {
		t.Helper()
		if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("agent-maintenance", a.node.ID, reservation) }); err != nil {
			t.Fatal(err)
		}
	}
	save()
	a.cancel()
	// A planned service shutdown closes its leases as well as its transport.
	// Check real I/O cancellation before allowing the Master fence to expire.
	a.service.Leases.Close()
	select {
	case <-lease.Done():
	case <-ctx.Done():
		t.Fatal("planned maintenance kept disconnected Agent authorized")
	}
	elapsed.Store(int64(runtime.leases.ttl + runtime.leases.margin + time.Second))
	if expired, err := runtime.leases.expired(p, a.node); err != nil || !expired {
		t.Fatalf("stopped Agent's conservative fence did not expire: expired=%v error=%v", expired, err)
	}
	p, err = runtime.reconcileFailover(ctx, p, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeRetired(a.node.ID) || p.ScannerNode().ID != a.node.ID || p.ScannerEpoch() != before.ScannerEpoch() || p.ScannedObjects != before.ScannedObjects || p.DispatchedThrough != before.DispatchedThrough {
		t.Fatalf("planned downtime changed the scan or its owner: %+v", p)
	}
	// Covers the final check too: maintenance can begin after the first check
	// and a slow shared-volume verification, before ownership is retired.
	p, err = runtime.retireUnavailableNode(p, a.node)
	if err != nil || p.NodeRetired(a.node.ID) {
		t.Fatalf("final retirement bypassed maintenance: %+v %v", p, err)
	}
	reservation.ExpiresAt = f.master.options.Now().Add(-time.Second)
	save()
	p, err = runtime.reconcileFailover(ctx, p, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.NodeRetired(a.node.ID) || p.ScannerNode().ID != b.node.ID || !p.Failover.ScannerSeeding {
		t.Fatalf("expired installer reservation blocked real failover: %+v", p)
	}
}
