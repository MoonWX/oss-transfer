package task

import (
	"math"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
)

// ApplyRequestLimits reserves a fixed share for every selected Agent, including
// offline Agents. An unavailable Agent's share must not be spent twice.
func ApplyRequestLimits(policy ratelimit.ParentLimit, spec Spec) ratelimit.ParentLimit {
	limits := spec.RequestLimits
	if limits == nil || len(spec.Nodes) == 0 {
		return policy
	}
	divisor := float64(len(spec.Nodes))
	if limits.Scope == "PER_NODE" {
		divisor = 1
	}
	policy.ListQPS = limits.ListQPS / divisor
	policy.HeadQPS = limits.HeadQPS / divisor
	policy.PutQPS = limits.PutQPS / divisor
	if limits.BandwidthBitsPerSecond > 0 {
		legacy := int64(math.Floor(limits.BandwidthBitsPerSecond / 8 / divisor))
		if legacy < 1 {
			// Invalid specs must be rejected by Agent admission, not converted
			// to the zero value that means unlimited throughput.
			policy.BandwidthBytesPerSecond = -1
			return policy
		}
		if legacy > 0 && (policy.BandwidthBytesPerSecond == 0 || legacy < policy.BandwidthBytesPerSecond) {
			policy.BandwidthBytesPerSecond = legacy
		}
	}
	return policy
}
