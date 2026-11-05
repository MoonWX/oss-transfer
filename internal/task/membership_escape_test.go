package task

import (
	"path/filepath"
	"testing"
)

// When the only surviving member is draining (here: being removed now), the
// failover of an offline peer must still finish by placing its work on that
// member, and the user must keep an API way out.
func TestFailoverToDrainingSurvivorFinishesAndRemovalCanBeCancelled(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _ := createFailoverTask(t, repo, testSpec(), 3) // all children on "a"
	p = enableFailoverTest(t, db, repo, p, false)
	p = changeNodes(t, repo, p, "now-b", nil, []string{"b"}, DrainModeNow)
	// a goes offline; its lease expires and failover starts with b the only
	// remaining (draining) member.
	p, err := repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil {
		t.Fatal(err)
	}
	moved, done, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true})
	if err != nil || !done || len(moved) != 3 {
		t.Fatal("failover to the draining survivor did not finish", len(moved), done, err)
	}
	if p, err = repo.Get(p.ID); err != nil || p.FailoverPending() {
		t.Fatal("failover still pending", err)
	}
	for _, child := range moved {
		if child.Node.ID != "b" {
			t.Fatal(child.Node)
		}
	}
	// Cancelling the immediate removal is always accepted.
	p = changeNodes(t, repo, p, "keep-b", []Node{{ID: "b", EnrollmentID: "eb"}}, nil, DrainModeDrain)
	if p.MembershipPending() || len(p.PlacementNodes()) != 1 {
		t.Fatal("removal not cancelled", p.Membership)
	}
}

// A disconnected peer can be removed while another change is pending.
func TestRetiredPeerCanBeRemovedWhileChangePending(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.Nodes = append(spec.Nodes, Node{ID: "c", EnrollmentID: "ec"})
	p, _ := createFailoverTask(t, repo, spec, 1)
	p = enableFailoverTest(t, db, repo, p, false)
	p, err := repo.BeginFailover(p.ID, p.Generation, "c")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "c", map[string]bool{"a": true, "b": true}); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Get(p.ID)
	p = changeNodes(t, repo, p, "drain-b", nil, []string{"b"}, DrainModeDrain)
	p = changeNodes(t, repo, p, "remove-c", nil, []string{"c"}, DrainModeDrain)
	if !p.NodeRemoved("c") {
		t.Fatal("retired peer not removed", p.Membership)
	}
	if _, _, err := repo.BeginNodeRejoin(p.ID, p.Generation, "c"); err == nil {
		t.Fatal("removed peer may still rejoin")
	}
}

// Joining nodes are not counted as remaining capacity.
func TestLastNodeIgnoresJoiners(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.Nodes = spec.Nodes[:1]
	p, _, err := repo.Create(spec, "last")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{DrainModeDrain, DrainModeNow} {
		change := NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}, Remove: []string{"a"}, Mode: mode}
		if code := membershipCode(p.CheckNodeChange(change)); code != "LAST_NODE" {
			t.Fatal(mode, code)
		}
	}
	// Add first; the original node can be removed once the joiner is active.
	p = changeNodes(t, repo, p, "add-c", []Node{{ID: "c", EnrollmentID: "ec"}}, nil, DrainModeDrain)
	if code := membershipCode(p.CheckNodeChange(NodeChange{Remove: []string{"a"}, Mode: DrainModeDrain})); code != "CHANGE_PENDING" {
		t.Fatal(code)
	}
	// Cancelling the join is always possible.
	p = changeNodes(t, repo, p, "cancel-c", nil, []string{"c"}, DrainModeDrain)
	if len(p.Spec.Nodes) != 1 || p.MembershipPending() {
		t.Fatal(p.Spec.Nodes, p.Membership)
	}
}

// Under affinity placement an added node would receive nothing yet halve the
// primary's share, so adding a node switches the task to balanced placement.
func TestAddToAffinityTaskSwitchesToBalancedAndMovesQueuedWork(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.Nodes = spec.Nodes[:1]
	spec.DistributionMode = "AFFINITY"
	p, _ := createFailoverTask(t, repo, spec, 4)
	p = changeNodes(t, repo, p, "add-c", []Node{{ID: "c", EnrollmentID: "ec"}}, nil, DrainModeDrain)
	if p.Spec.DistributionMode != "BALANCED" {
		t.Fatal(p.Spec.DistributionMode)
	}
	p = enableFailoverTest(t, db, repo, p, false)
	p = joinNode(t, repo, p, "c")
	available := map[string]bool{"a": true, "c": true}
	moved := 0
	for range 40 {
		intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, available)
		if err != nil {
			t.Fatal(err)
		}
		if intent != nil {
			if c, err := repo.FinishQueuedHandoff(p.ID, *intent, true, available); err != nil {
				t.Fatal(err)
			} else if c != nil {
				moved++
			}
		}
		if p, err = repo.Get(p.ID); err != nil {
			t.Fatal(err)
		}
		if p.Failover.Rebalance == nil {
			break
		}
	}
	if moved == 0 {
		t.Fatal("added node received no queued work")
	}
}
