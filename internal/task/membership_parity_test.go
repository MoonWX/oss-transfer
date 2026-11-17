package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// The dry run of a node change (CheckNodeChange) and its commit (ChangeNodes)
// count the nodes that share a CLUSTER bandwidth limit alike: every member
// that has not been removed. A removed node is fenced for good; a node lost to
// takeover still counts, because it rejoins when it reconnects.
func TestNodeChangeDryRunAndCommitShareBandwidthAlike(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec() // nodes a and b
	spec.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", BandwidthBitsPerSecond: 16}
	create := func(key string) Parent {
		t.Helper()
		p, _, err := repo.Create(spec, key)
		if err != nil {
			t.Fatal(err)
		}
		return enableFailoverTest(t, db, repo, p, false)
	}
	// agree adds node c as a dry run and as a commit; both must answer want.
	agree := func(p Parent, key, want string) {
		t.Helper()
		change := NodeChange{Add: []Node{{ID: "c", EnrollmentID: "ec"}}, Mode: DrainModeDrain, RequestKey: key, Fingerprint: key}
		dry := p.CheckNodeChange(change)
		_, _, commit := repo.ChangeNodes(p.ID, change)
		if membershipCode(dry) != want || membershipCode(commit) != want || want == "" && (dry != nil || commit != nil) {
			t.Fatalf("%s: dry run %v, commit %v, want %q", key, dry, commit, want)
		}
	}
	// a, b and c would get two thirds of a byte per second each.
	agree(create("all"), "all", "BANDWIDTH")

	lost := create("lost")
	lost, err := repo.BeginFailover(lost.ID, lost.Generation, "b")
	if err != nil || !lost.NodeRetired("b") || lost.NodeRemoved("b") {
		t.Fatal("b was not taken over", err, lost.Failover)
	}
	agree(lost, "lost", "BANDWIDTH")

	removed := create("removed")
	removed = changeNodes(t, repo, removed, "remove-b", nil, []string{"b"}, DrainModeDrain)
	if ready, err := repo.DrainReady(removed.ID, "b"); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if removed, err = repo.BeginFailover(removed.ID, removed.Generation, "b"); err != nil || !removed.NodeRemoved("b") {
		t.Fatal("b did not leave", err, removed.Membership)
	}
	// Only a and c share the 2 bytes per second now.
	agree(removed, "removed", "")
	// Editing the limits counts the same nodes.
	if _, err := repo.SetLimits(removed.ID, nil, spec.RequestLimits, true); err != nil {
		t.Fatal("the limit that a and c share was refused", err)
	}
}
