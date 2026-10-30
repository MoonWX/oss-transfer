package master

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Real authenticated Agent RPCs exercise the durable Master/Agent handoff.
// Returning a node must enable only new work, without stealing the replacement's
// assignment or reactivating an old report/lease after a Master restart.
func TestReturningNodeRejoinsWithFreshEpochAndFutureAssignments(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "rejoin-a"), takeoverTestAgent(t, f, "rejoin-b")
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl = time.Second
	runtime.leases.margin = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 1, Objects: []transferstore.Object{{Key: "old", Size: 1}}}}, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	old := rows[0]
	p, err = runtime.reconcileFailover(ctx, p, map[string]bool{a.node.ID: true, b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.service.Leases.Retire(ctx, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	e, err := runtime.leases.entry(p, b.node)
	if err != nil {
		t.Fatal(err)
	}
	// Both parties are quiescent; advancing only this fixture's conservative
	// clock avoids sleeping while retaining the retirement precondition.
	e.mu.Lock()
	e.safeAfter = time.Now().Add(-time.Second)
	e.mu.Unlock()
	if err = runtime.leases.retire(p, b.node); err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.BeginFailover(p.ID, p.Generation, b.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := runtime.repo.PrepareFailover(p.ID, p.Generation, b.node.ID, map[string]bool{a.node.ID: true}); err != nil || !done {
		t.Fatal(done, err)
	}
	if _, err = b.service.Jobs.Accept(ctx, old.Job); err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.rejoinAvailableNodes(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeRetired(b.node.ID) || p.NodeEpoch(b.node.ID) != 1 || p.Generation != 1 {
		t.Fatal("healthy returner did not rejoin", p.Failover)
	}
	if err = runtime.leases.renew(ctx, p, b.node); err != nil {
		t.Fatal(err)
	}
	if _, err = b.service.Leases.JobContext(old.Job); !errors.Is(err, agentservice.ErrLeaseConflict) {
		t.Fatal("old assignment authorized", err)
	}
	if err = b.service.Leases.Retire(ctx, p.ID, 0); !errors.Is(err, agentservice.ErrLeaseConflict) {
		t.Fatal("delayed old retirement revoked new work", err)
	}
	next, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, []task.Assignment{{Number: 2, Objects: []transferstore.Object{{Key: "new", Size: 1}}}}, map[string]bool{b.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if next[0].Node.ID != b.node.ID || next[0].Job.OwnerEpoch != 1 {
		t.Fatal("fresh job missing node epoch", next)
	}
	if _, err = b.service.Leases.JobContext(next[0].Job); err != nil {
		t.Fatal("fresh assignment refused", err)
	}
	rows, err = runtime.repo.Children(p.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Node.ID != a.node.ID || rows[0].Job.Assignment != 1 || rows[0].Job.OwnerEpoch != 0 {
		t.Fatal("rejoin stole replacement work", rows[0])
	}
	if err = runtime.repo.Observe(p.ID, p.Generation, 1, execution.Record{Job: old.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now()}); err == nil {
		t.Fatal("late old report accepted")
	}
	if pending, _, err := b.service.Jobs.Unfinished(); err != nil || pending != 0 {
		t.Fatal("stale queue held capacity", pending, err)
	}
	// Recreated Master lease state must retain the current ownership epoch.
	fresh := newTaskLeases(runtime)
	if err = fresh.renew(ctx, p, b.node); err != nil {
		t.Fatal("Master restart lost current epoch", err)
	}
	if _, err = b.service.Leases.JobContext(old.Job); !errors.Is(err, agentservice.ErrLeaseConflict) {
		t.Fatal("Master restart revived old ownership", err)
	}
	p, err = runtime.repo.Stop(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.stopMembers(ctx, p); err != nil {
		t.Fatal(err)
	}
	if ready, err := runtime.observeStopped(ctx, p); err != nil || !ready {
		t.Fatal("retired queue blocked task stop", ready, err)
	}
	if err = runtime.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, _, err = runtime.repo.RequestResume(p.ID, nil)
	if err != nil {
		t.Fatal("rejoined task could not resume", err)
	}
	if done, err := runtime.repo.PrepareResume(p.ID, p.Generation); err != nil || !done {
		t.Fatal(done, err)
	}
	resumed, err := runtime.repo.Children(p.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if resumed[0].Job.OwnerEpoch != 0 || resumed[1].Job.OwnerEpoch != 1 {
		t.Fatal("resume lost ownership epochs", resumed)
	}
}

func TestRejoinRecoversLostMasterFinishAcknowledgement(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "rejoin-replay-a"), takeoverTestAgent(t, f, "rejoin-replay-b")
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := runtime.repo.EnableFailover(p.ID, p.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.BeginFailover(p.ID, p.Generation, b.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := runtime.repo.PrepareFailover(p.ID, p.Generation, b.node.ID, map[string]bool{a.node.ID: true}); err != nil || !done {
		t.Fatal(done, err)
	}
	var epoch uint64
	p, epoch, err = runtime.repo.BeginNodeRejoin(p.ID, p.Generation, b.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.leases.rejoin(ctx, p, b.node, epoch); err != nil {
		t.Fatal(err)
	}
	if expired, err := runtime.leases.expired(p, b.node); err != nil || !expired {
		t.Fatal("ACKed inactive epoch cannot retire after immediate disconnect", expired, err)
	}
	// Master crashes after the Agent ACK/lease commit, before clearing retirement.
	runtime.leases = newTaskLeases(runtime)
	p, err = runtime.rejoinAvailableNodes(ctx, p)
	if err != nil || p.NodeRetired(b.node.ID) || p.NodeEpoch(b.node.ID) != epoch {
		t.Fatal("partial transaction was not reconciled", p.Failover, err)
	}
	if err = runtime.leases.renew(ctx, p, b.node); err != nil {
		t.Fatal(err)
	}
}

func TestRejoinDoesNotGrantBeforeExistingOwnersAcknowledgeRateShare(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "rejoin-budget-a"), takeoverTestAgent(t, f, "rejoin-budget-b")
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := runtime.repo.EnableFailover(p.ID, p.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.BeginFailover(p.ID, p.Generation, b.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := runtime.repo.PrepareFailover(p.ID, p.Generation, b.node.ID, map[string]bool{a.node.ID: true}); err != nil || !done {
		t.Fatal(done, err)
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.rejoinAvailableNodes(ctx, p)
	if err != nil || p.NodeRetired(b.node.ID) {
		t.Fatal(p.Failover, err)
	}
	a.cancel()
	attempt, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if err = runtime.leases.renew(attempt, p, b.node); err == nil {
		t.Fatal("returner granted a lease without reduced-share acknowledgement")
	}
	if _, err = b.service.Leases.JobContext(execution.Job{ParentID: p.ID, Generation: p.Generation, OwnerEpoch: 1}); err == nil {
		t.Fatal("returner executing while old owner may retain full task limit")
	}
}
