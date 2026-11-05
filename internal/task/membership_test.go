package task

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func membershipCode(err error) string {
	var rejection *MembershipError
	if errors.As(err, &rejection) {
		return rejection.Code
	}
	return ""
}

func changeNodes(t *testing.T, repo *Repository, p Parent, key string, add []Node, remove []string, mode string) Parent {
	t.Helper()
	p, replayed, err := repo.ChangeNodes(p.ID, NodeChange{Add: add, Remove: remove, Mode: mode, RequestKey: key, Fingerprint: key})
	if err != nil || replayed {
		t.Fatal(replayed, err)
	}
	return p
}

func assignOne(t *testing.T, repo *Repository, p Parent, number int, available map[string]bool) Child {
	t.Helper()
	rows, err := repo.AssignPageAvailable(p.ID, p.Generation, []Assignment{{Number: number, Objects: schedulingObjects("n"+string(rune('a'+number%26))+string(rune('0'+number/26)), 2, 100)}}, available)
	if err != nil {
		t.Fatal(err)
	}
	return rows[0]
}

func joinNode(t *testing.T, repo *Repository, p Parent, id string) Parent {
	t.Helper()
	p, epoch, err := repo.BeginNodeRejoin(p.ID, p.Generation, id)
	if err != nil {
		t.Fatal(err)
	}
	if p, err = repo.FinishNodeRejoin(p.ID, p.Generation, id, epoch); err != nil {
		t.Fatal(err)
	}
	return p
}

// An Agent added to a single-node task during its scan gets no authority or
// placement until it joins; afterwards it shares queued and future work.
func TestAddNodeDuringScanJoinsPlacementAfterRejoin(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.Nodes = spec.Nodes[:1]
	p, _ := createFailoverTask(t, repo, spec, 4)
	revision := p.BudgetRevision
	p = changeNodes(t, repo, p, "add-c", []Node{{ID: "c", EnrollmentID: "ec", Address: "c"}}, nil, DrainModeDrain)
	if !p.NodeJoining("c") || !p.NodeRetired("c") || len(p.ActiveNodes()) != 1 || p.NodeRevision() != 1 || p.BudgetRevision != revision {
		t.Fatal("joining node was activated early", p.Membership, p.ActiveNodes())
	}
	if child := assignOne(t, repo, p, 5, map[string]bool{"a": true, "c": true}); child.Node.ID != "a" {
		t.Fatal("joining node received work", child.Node)
	}
	p = enableFailoverTest(t, db, repo, p, false)
	p = joinNode(t, repo, p, "c")
	if p.NodeJoining("c") || p.NodeRetired("c") || len(p.ActiveNodes()) != 2 || p.BudgetRevision != revision+1 || p.NodeEpoch("c") != 1 {
		t.Fatal("join did not take effect", p.Membership, p.Failover)
	}
	available := map[string]bool{"a": true, "c": true}
	moved := 0
	for range 40 {
		intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, available)
		if err != nil {
			t.Fatal(err)
		}
		if intent != nil {
			c, err := repo.FinishQueuedHandoff(p.ID, *intent, true, available)
			if err != nil {
				t.Fatal(err)
			}
			if c != nil {
				moved++
				if c.Node.ID != "c" || c.Job.OwnerEpoch != 1 {
					t.Fatal(c)
				}
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
		t.Fatal("queued children were not rebalanced to the added node")
	}
	if child := assignOne(t, repo, p, 6, available); child.Node.ID != "c" {
		t.Fatal("future child was not placed on the lighter added node", child.Node, p.Scheduling)
	}
}

// Draining stops placement, hands off every queued child regardless of
// balance, waits for running children, then fences the node permanently.
func TestRemoveDrainHandsOffQueueWaitsAndFences(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, children := createFailoverTask(t, repo, testSpec(), 3) // all on "a"
	p = enableFailoverTest(t, db, repo, p, false)
	running := children[0]
	if err := repo.Observe(p.ID, p.Generation, running.Job.Number, schedulingReport(running, 1)); err != nil {
		t.Fatal(err)
	}
	if p, err := repo.Get(p.ID); err != nil {
		t.Fatal(err)
	} else {
		p = changeNodes(t, repo, p, "drain-a", nil, []string{"a"}, DrainModeDrain)
	}
	p, _ = repo.Get(p.ID)
	if child := assignOne(t, repo, p, 4, map[string]bool{"a": true, "b": true}); child.Node.ID != "b" {
		t.Fatal("draining node received new work", child.Node)
	}
	p, err := repo.EnsureDrainRebalance(p.ID, p.Generation)
	if err != nil || p.Failover.Rebalance == nil {
		t.Fatal("drain did not arm handoff", err)
	}
	available := map[string]bool{"a": true, "b": true}
	for range 20 {
		intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, available)
		if err != nil {
			t.Fatal(err)
		}
		if intent != nil {
			if _, err := repo.FinishQueuedHandoff(p.ID, *intent, true, available); err != nil {
				t.Fatal(err)
			}
		}
		if p, err = repo.Get(p.ID); err != nil {
			t.Fatal(err)
		}
		if p.Failover.Rebalance == nil {
			break
		}
	}
	rows, err := repo.Children(p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Job.Number != running.Job.Number && row.Node.ID != "b" {
			t.Fatal("queued child stayed on the draining node", row.Job.Number, row.Node.ID)
		}
	}
	if rows[0].Node.ID != "a" {
		t.Fatal("running child was moved", rows[0].Node)
	}
	if ready, err := repo.DrainReady(p.ID, "a"); err != nil || ready {
		t.Fatal("drain finished while a child was running", ready, err)
	}
	if err := repo.Observe(p.ID, p.Generation, running.Job.Number, schedulingReport(running, 3)); err != nil {
		t.Fatal(err)
	}
	if ready, err := repo.DrainReady(p.ID, "a"); err != nil || !ready {
		t.Fatal("drain did not finish", ready, err)
	}
	before, _ := repo.Get(p.ID)
	p, err = repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !p.NodeRemoved("a") || p.MembershipPending() || len(p.ActiveNodes()) != 1 || p.BudgetRevision != before.BudgetRevision+1 {
		t.Fatal("node did not leave", p.Membership)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); err != nil {
		t.Fatal(err)
	}
	if p, _, err = repo.BeginNodeRejoin(p.ID, p.Generation, "a"); !errors.Is(err, ErrState) {
		t.Fatal("removed node can rejoin automatically", err)
	}
	if p.Counters.Succeeded != 3 {
		t.Fatal("accounting changed", p.Counters)
	}
}

// Removing now moves a partly transferred child with its durable progress;
// the removed owner's late report is rejected (Master-side fencing).
func TestRemoveNowKeepsProgressAndRejectsLateReports(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, children := createFailoverTask(t, repo, testSpec(), 1)
	p = enableFailoverTest(t, db, repo, p, false)
	child := children[0]
	partial := failoverRecord(child, false)
	partial.Status = execution.Running
	partial.FinishedAt = nil
	if err := repo.Observe(p.ID, p.Generation, child.Job.Number, partial); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Get(p.ID)
	p = changeNodes(t, repo, p, "now-a", nil, []string{"a"}, DrainModeNow)
	if d, ok := p.NodeDraining("a"); !ok || d.Mode != DrainModeNow {
		t.Fatal(p.Membership)
	}
	p, err := repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); err != nil || !done {
		t.Fatal(done, err)
	}
	p, _ = repo.Get(p.ID)
	rows, _ := repo.Children(p.ID, 0, 1)
	if rows[0].Node.ID != "b" || rows[0].Record.Counters.Succeeded != 1 || rows[0].Record.Counters.Skipped != 1 || p.Counters.Succeeded != 1 || p.Counters.Skipped != 1 || p.Counters.Failed != 0 {
		t.Fatal("progress was not carried over exactly once", rows[0].Record.Counters, p.Counters)
	}
	late := failoverRecord(child, true)
	late.Attempt = 99
	if err := repo.Observe(p.ID, p.Generation, child.Job.Number, late); err == nil {
		t.Fatal("removed node's late report was accepted")
	}
	after, _ := repo.Get(p.ID)
	if after.Counters != p.Counters || after.FinishedChildren != 0 {
		t.Fatal("late report changed accounting", after.Counters)
	}
}

// The scanner may drain; when it leaves, scanning moves by checkpoint takeover.
func TestRemovingScannerMovesScan(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "scanner")
	if err != nil {
		t.Fatal(err)
	}
	p = enableFailoverTest(t, db, repo, p, false)
	if p.ScannerNode().ID != "a" {
		t.Fatal(p.ScannerNode())
	}
	p = changeNodes(t, repo, p, "drain-scanner", nil, []string{"a"}, DrainModeDrain)
	if ready, err := repo.DrainReady(p.ID, "a"); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if p, err = repo.BeginFailover(p.ID, p.Generation, "a"); err != nil {
		t.Fatal(err)
	}
	if p, err = repo.BeginScannerFailover(p.ID, p.Generation, "b"); err != nil {
		t.Fatal(err)
	}
	if p.ScannerNode().ID != "b" || !p.Failover.ScannerSeeding || p.ScannerEpoch() != 1 {
		t.Fatal("scan did not move", p.Failover)
	}
}

func TestNodeChangesAreIdempotentDurableAndSchemaFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	p, _, err := repo.Create(testSpec(), "idempotent")
	if err != nil {
		t.Fatal(err)
	}
	change := NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}, Mode: DrainModeDrain, RequestKey: "req-1", Fingerprint: "f1"}
	if _, replayed, err := repo.ChangeNodes(p.ID, change); err != nil || replayed {
		t.Fatal(replayed, err)
	}
	if again, replayed, err := repo.ChangeNodes(p.ID, change); err != nil || !replayed || again.NodeRevision() != 1 || len(again.Spec.Nodes) != 3 {
		t.Fatal("replay applied twice", replayed, err, again.Spec.Nodes)
	}
	if seen, err := repo.NodeRequestSeen(p.ID, "req-1", "f1"); err != nil || !seen {
		t.Fatal(seen, err)
	}
	other := change
	other.Fingerprint = "f2"
	if _, _, err := repo.ChangeNodes(p.ID, other); !errors.Is(err, ErrConflict) {
		t.Fatal("request key reused for another change", err)
	}
	stale := uint64(0)
	if _, _, err := repo.ChangeNodes(p.ID, NodeChange{Remove: []string{"c"}, Mode: DrainModeDrain, RequestKey: "req-2", Fingerprint: "x", ExpectedRevision: &stale}); membershipCode(err) != "STALE" {
		t.Fatal("stale revision accepted", err)
	}
	db.Close()
	db, repo = openTest(t, path)
	defer db.Close()
	p, err = repo.Get(p.ID)
	if err != nil || !p.NodeJoining("c") || p.NodeRevision() != 1 {
		t.Fatal("membership not durable", p.Membership, err)
	}
	var format struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := db.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &format); return err }); err != nil || format.SchemaVersion < state.SchemaTaskMembership {
		t.Fatal("schema not raised", format, err)
	}
}

func TestNodeChangeRejectionsAndPendingPolicy(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "rejections")
	if err != nil {
		t.Fatal(err)
	}
	check := func(p Parent, change NodeChange, code string) {
		t.Helper()
		if change.Mode == "" {
			change.Mode = DrainModeDrain
		}
		if got := membershipCode(p.CheckNodeChange(change)); got != code {
			t.Fatalf("expected %q, got %q (%v)", code, got, p.CheckNodeChange(change))
		}
	}
	check(p, NodeChange{Remove: []string{"a", "b"}}, "LAST_NODE")
	check(p, NodeChange{Remove: []string{"z"}}, "NOT_MEMBER")
	check(p, NodeChange{Add: []Node{{ID: "a", EnrollmentID: "ea"}}}, "ALREADY_MEMBER")
	single := p
	single.Spec.ObjectKey = "one"
	check(single, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "SINGLE_OBJECT")
	done := p
	done.Status = "COMPLETED"
	check(done, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "COMPLETED")
	stopping := p
	stopping.Status = "STOPPING"
	check(stopping, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "TRANSITION")
	deleted := p
	deleted.Deleted = true
	check(deleted, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "DELETED")
	legacy := p
	legacy.Legacy = &LegacyProvenance{}
	check(legacy, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "LEGACY")
	narrow := p
	narrow.Spec.BandwidthMbps = 0.000016 // two bytes per second
	check(narrow, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "BANDWIDTH")
	upload := p
	upload.Spec.Direction, upload.Spec.Source, upload.Spec.Destination = "LOCAL_TO_OSS", execution.Endpoint{Kind: "LOCAL"}, execution.Endpoint{Kind: "S3", Ref: "t", Bucket: "b"}
	check(upload, NodeChange{Remove: []string{"b"}, Mode: DrainModeNow}, "NOW_UNSUPPORTED")
	stopped := p
	stopped.Status = "STOPPED"
	check(stopped, NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}}, "")

	p = changeNodes(t, repo, p, "pending", []Node{{ID: "c", EnrollmentID: "ec"}}, []string{"b"}, DrainModeDrain)
	check(p, NodeChange{Add: []Node{{ID: "d", EnrollmentID: "ed"}}}, "CHANGE_PENDING")
	check(p, NodeChange{Remove: []string{"a"}}, "CHANGE_PENDING")
	check(p, NodeChange{Remove: []string{"b"}}, "CHANGE_PENDING")
	check(p, NodeChange{Remove: []string{"b"}, Mode: DrainModeNow}, "")
	check(p, NodeChange{Add: []Node{{ID: "b", EnrollmentID: "eb"}}}, "")
	// Cancelling the join forgets a node that never held authority.
	p = changeNodes(t, repo, p, "cancel-join", nil, []string{"c"}, DrainModeDrain)
	if len(p.Spec.Nodes) != 2 || p.NodeJoining("c") {
		t.Fatal("join not cancelled", p.Spec.Nodes, p.Membership)
	}
	// Cancelling the drain keeps the node.
	p = changeNodes(t, repo, p, "cancel-drain", []Node{{ID: "b", EnrollmentID: "eb"}}, nil, DrainModeDrain)
	if p.MembershipPending() || len(p.PlacementNodes()) != 2 {
		t.Fatal("drain not cancelled", p.Membership)
	}
}

func TestCompletionSettlesPendingMembership(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, children := createFailoverTask(t, repo, testSpec(), 1)
	p = changeNodes(t, repo, p, "settle", []Node{{ID: "c", EnrollmentID: "ec"}}, []string{"b"}, DrainModeDrain)
	if err := repo.ScanProgress(p.ID, p.Generation, 3, 300, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Observe(p.ID, p.Generation, 1, schedulingReport(children[0], 3)); err != nil {
		t.Fatal(err)
	}
	if finished, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || !finished {
		t.Fatal(finished, err)
	}
	p, _ = repo.Get(p.ID)
	if p.Status != "COMPLETED" || p.MembershipPending() || !p.NodeRemoved("b") || len(p.CurrentNodes()) != 1 {
		t.Fatal("pending membership survived completion", p.Membership, p.CurrentNodes())
	}
}
