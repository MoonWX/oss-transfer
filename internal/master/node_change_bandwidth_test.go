package master

import (
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// The dry run and the commit of a node change count the nodes that share the
// task's bandwidth limits alike: once B has been removed, C may join a CLUSTER
// limit of 16 bits per second that A and C then share.
func TestNodeChangeDryRunAndCommitAgreeOnBandwidthShares(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b, c := takeoverTestAgent(t, f, "share-a"), takeoverTestAgent(t, f, "share-b"), takeoverTestAgent(t, f, "share-c")
	waitOnline(t, f, a.node, b.node, c.node)
	repo := f.master.tasks.repo
	p, _, err := repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{a.node, b.node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", RequestLimits: &execution.RequestLimits{Scope: "CLUSTER", BandwidthBitsPerSecond: 16}}, "cluster-bandwidth")
	if err != nil {
		t.Fatal(err)
	}
	addC := func(dryRun bool) (int, map[string]any) {
		return postNodes(t, f, p.ID, map[string]any{"add": []string{c.node.Address}, "dryRun": dryRun})
	}
	for _, dryRun := range []bool{true, false} {
		if status, body := addC(dryRun); status != 409 || body["code"] != "BANDWIDTH" {
			t.Fatal("A, B and C cannot share 2 bytes per second", dryRun, status, body)
		}
	}
	if _, _, err := repo.ChangeNodes(p.ID, task.NodeChange{Remove: []string{b.node.ID}, Mode: task.DrainModeDrain, RequestKey: "remove-b", Fingerprint: "remove-b"}); err != nil {
		t.Fatal(err)
	}
	if p, err = repo.EnableFailover(p.ID, p.Generation, false); err != nil {
		t.Fatal(err)
	}
	if ready, err := repo.DrainReady(p.ID, b.node.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if p, err = repo.BeginFailover(p.ID, p.Generation, b.node.ID); err != nil || !p.NodeRemoved(b.node.ID) {
		t.Fatal("B did not leave", err)
	}
	if status, body := addC(true); status != 200 || body["data"].(map[string]any)["valid"] != true {
		t.Fatal("the dry run refused C", status, body)
	}
	if status, body := addC(false); status != 200 || body["data"].(map[string]any)["accepted"] != true {
		t.Fatal("the dry run passed, but the change was refused", status, body)
	}
}
