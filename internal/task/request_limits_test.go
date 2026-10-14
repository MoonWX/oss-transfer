package task

import (
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
)

func TestRequestLimitAllocationPreservesLegacyScopeAndTighterBandwidth(t *testing.T) {
	spec := Spec{Nodes: []Node{{ID: "one"}, {ID: "two"}}, RequestLimits: &execution.RequestLimits{Scope: "PER_NODE", ListQPS: 100, HeadQPS: 2800, PutQPS: 1400, BandwidthBitsPerSecond: 2e9}}
	policy := ratelimit.ParentLimit{ParentID: 7, Generation: 2, Revision: 3}
	perNode := ApplyRequestLimits(policy, spec)
	if perNode.ListQPS != 100 || perNode.HeadQPS != 2800 || perNode.PutQPS != 1400 || perNode.BandwidthBytesPerSecond != 250000000 {
		t.Fatal("legacy per-node caps changed", perNode)
	}
	spec.RequestLimits.Scope = "CLUSTER"
	cluster := ApplyRequestLimits(policy, spec)
	if cluster.ListQPS*2 != 100 || cluster.HeadQPS*2 != 2800 || cluster.PutQPS*2 != 1400 || cluster.BandwidthBytesPerSecond*2 != 250000000 {
		t.Fatal("cluster cap duplicated across selected agents", cluster)
	}
	policy.BandwidthBytesPerSecond = 1250000
	combined := ApplyRequestLimits(policy, spec)
	if combined.BandwidthBytesPerSecond != 1250000 {
		t.Fatal("legacy bandwidth weakened global task cap", combined)
	}
	if combined.ParentID != 7 || combined.Generation != 2 || combined.Revision != 3 {
		t.Fatal("allocation changed identity")
	}
	// A corrupted/unchecked tiny allocation must never turn zero into unlimited.
	spec.RequestLimits.BandwidthBitsPerSecond = 8
	if ApplyRequestLimits(policy, spec).BandwidthBytesPerSecond >= 0 {
		t.Fatal("tiny positive bandwidth did not fail closed")
	}
}
