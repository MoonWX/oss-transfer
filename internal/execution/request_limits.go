package execution

import (
	"errors"
	"math"
)

// PER_NODE preserves V4 local-task policies. New policies use CLUSTER, with
// shares allocated by Master before any Agent accepts execution or discovery.
type RequestLimits struct {
	ListQPS                float64 `json:"listQps,omitempty"`
	HeadQPS                float64 `json:"headQps,omitempty"`
	PutQPS                 float64 `json:"putQps,omitempty"`
	BandwidthBitsPerSecond float64 `json:"bandwidthBitsPerSecond,omitempty"`
	Scope                  string  `json:"scope"`
}

func ValidateRequestLimits(limits *RequestLimits) error {
	if limits == nil {
		return nil
	}
	if limits.Scope != "" && limits.Scope != "CLUSTER" && limits.Scope != "PER_NODE" {
		return errors.New("request limit scope must be CLUSTER or PER_NODE")
	}
	for _, value := range []float64{limits.ListQPS, limits.HeadQPS, limits.PutQPS} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e9 || value > 0 && value < 1e-6 {
			return errors.New("request limits must be zero or between 0.000001 and 1000000000 requests per second")
		}
	}
	bandwidth := limits.BandwidthBitsPerSecond
	if math.IsNaN(bandwidth) || math.IsInf(bandwidth, 0) || bandwidth < 0 || bandwidth > 8*(1<<50) || bandwidth > 0 && bandwidth < 8 {
		return errors.New("legacy bandwidth limit must be zero or between 8 and 9007199254740992 bits per second")
	}
	return nil
}
