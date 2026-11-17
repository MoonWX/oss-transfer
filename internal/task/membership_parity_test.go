package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
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

// A node change records an explicit skip of the shared directory check only
// when it adds a local path. A removal, a cancelled removal or a task without
// a local endpoint ignores it. The record raises the database format, as at
// creation, so that an older Master cannot drop it.
func TestNodeChangeRecordsASkippedSharedCheckForAddedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	spec := testSpec()
	spec.Direction, spec.Source, spec.TargetPrefix, spec.SharedLocalSource = "LOCAL_TO_OSS", execution.Endpoint{Kind: "LOCAL", Root: "/data"}, "", true
	for i := range spec.Nodes {
		spec.Nodes[i].LocalRoot = "/data"
	}
	p, _, err := repo.Create(spec, "local")
	if err != nil {
		t.Fatal(err)
	}
	skip := func(p Parent, key string, add []Node, remove []string) Parent {
		t.Helper()
		p, replayed, err := repo.ChangeNodes(p.ID, NodeChange{Add: add, Remove: remove, Mode: DrainModeDrain, RequestKey: key, Fingerprint: key, SkipSharedCheck: true})
		if err != nil || replayed {
			t.Fatal(key, replayed, err)
		}
		return p
	}
	schema := func() int {
		t.Helper()
		var format struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if err := db.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &format); return err }); err != nil {
			t.Fatal(err)
		}
		return format.SchemaVersion
	}
	p = skip(p, "drain", nil, []string{"b"})
	p = skip(p, "keep", []Node{p.Spec.Nodes[1]}, nil) // cancels the drain
	if p.Spec.SharedCheckSkipped || p.MembershipPending() || schema() >= state.SchemaSharedCheckSkipped {
		t.Fatal("a change that adds no path recorded a skipped check", p.Membership, schema())
	}
	p = skip(p, "join", []Node{{ID: "c", EnrollmentID: "ec", LocalRoot: "/mnt/c"}}, nil)
	if !p.Spec.SharedCheckSkipped || !p.NodeJoining("c") || schema() < state.SchemaSharedCheckSkipped {
		t.Fatal("an added path did not record the skipped check", p.Spec.SharedCheckSkipped, schema())
	}
	// Only the added node goes unverified; the members stay verified.
	if !p.Spec.SharedCheckSkippedNodesOnly || p.SharedCheckSkippedForEveryNode() || !p.SharedCheckSkippedFor("c") || p.SharedCheckSkippedFor("a") || p.SharedCheckSkippedFor("b") {
		t.Fatal("the skip does not cover exactly the added node", p.Spec.Nodes)
	}
	db.Close()
	db, repo = openTest(t, path)
	defer db.Close()
	if p, err = repo.Get(p.ID); err != nil || !p.Spec.SharedCheckSkipped || !p.SharedCheckSkippedFor("c") || p.SharedCheckSkippedFor("a") {
		t.Fatal("the skipped check is not durable", err)
	}
	buckets, _, err := repo.Create(testSpec(), "buckets")
	if err != nil {
		t.Fatal(err)
	}
	if buckets = skip(buckets, "buckets-join", []Node{{ID: "c", EnrollmentID: "ec"}}, nil); buckets.Spec.SharedCheckSkipped {
		t.Fatal("a task without a local endpoint recorded a skipped check")
	}
}

// A skip made at creation covers every node, also those that node changes
// add later, verified or not. A skip made by a node change covers the nodes
// it adds: a node added again without the skip is verified again, and the
// task keeps showing that the check was skipped.
func TestSkippedSharedCheckCoversTheNodesItWasMadeFor(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.Direction, spec.Source, spec.TargetPrefix, spec.SharedLocalSource = "LOCAL_TO_OSS", execution.Endpoint{Kind: "LOCAL", Root: "/data"}, "", true
	for i := range spec.Nodes {
		spec.Nodes[i].LocalRoot = "/data"
	}
	change := func(p Parent, key string, skip bool, add []Node, remove []string) Parent {
		t.Helper()
		p, _, err := repo.ChangeNodes(p.ID, NodeChange{Add: add, Remove: remove, Mode: DrainModeDrain, RequestKey: key, Fingerprint: key, SkipSharedCheck: skip})
		if err != nil {
			t.Fatal(key, err)
		}
		return p
	}
	created := spec
	created.SharedCheckSkipped = true
	everyNode, _, err := repo.Create(created, "created-skipped")
	if err != nil {
		t.Fatal(err)
	}
	everyNode = change(everyNode, "verified-join", false, []Node{{ID: "c", EnrollmentID: "ec", LocalRoot: "/data"}}, nil)
	for _, id := range []string{"a", "b", "c"} {
		if !everyNode.SharedCheckSkippedForEveryNode() || !everyNode.SharedCheckSkippedFor(id) {
			t.Fatal("a skip made at creation does not cover every node", id, everyNode.Spec)
		}
	}
	everyNode = change(everyNode, "cancel-c", false, nil, []string{"c"})
	everyNode = change(everyNode, "unchecked-join", true, []Node{{ID: "d", EnrollmentID: "ed", LocalRoot: "/mnt/d"}}, nil)
	if !everyNode.SharedCheckSkippedForEveryNode() || !everyNode.SharedCheckSkippedFor("a") || !everyNode.SharedCheckSkippedFor("d") {
		t.Fatal("a skipping node change limited a skip made at creation", everyNode.Spec)
	}

	verified, _, err := repo.Create(spec, "verified")
	if err != nil {
		t.Fatal(err)
	}
	c := Node{ID: "c", EnrollmentID: "ec", LocalRoot: "/mnt/c"}
	verified = change(verified, "add-c", true, []Node{c}, nil)
	verified = change(verified, "cancel-c", false, nil, []string{"c"})
	verified = change(verified, "add-c-again", false, []Node{c}, nil)
	if verified.SharedCheckSkippedFor("c") || !verified.NodeJoining("c") || !verified.Spec.SharedCheckSkipped {
		t.Fatal("a node added again without the skip is not verified, or the task forgot the skip", verified.Spec)
	}
}
