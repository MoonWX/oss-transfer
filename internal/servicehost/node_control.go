package servicehost

import (
	"errors"
	"math"
	"time"

	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
)

// Pointer fields distinguish omitted defaults from explicit zero (unlimited).
// This wire type deliberately excludes timers, functions and controller state.
type NodeControlConfig struct {
	ListQPS                *float64          `json:"listQps,omitempty"`
	PutQPS                 *float64          `json:"putQps,omitempty"`
	HeadQPS                *float64          `json:"headQps,omitempty"`
	BandwidthBitsPerSecond *float64          `json:"bandwidthBitsPerSecond,omitempty"`
	AutoAdjust             *AutoAdjustConfig `json:"autoAdjust,omitempty"`
}

type AutoAdjustConfig struct {
	Consecutive503Threshold *int     `json:"consecutive503Threshold,omitempty"`
	ReduceFactor            *float64 `json:"reduceFactor,omitempty"`
	IncreaseFactor          *float64 `json:"increaseFactor,omitempty"`
	MinQPS                  *float64 `json:"minQps,omitempty"`
	RecoveryIntervalMS      *int64   `json:"recoveryIntervalMs,omitempty"`
}

// Options preserves the current no-config defaults. A migration must supply
// resolved effective V4 values; absence here is not a claim about an old node.
func (c *NodeControlConfig) Options() (nodecontrol.Options, error) {
	out := nodecontrol.Options{ListQPS: 100, PutQPS: 1400, HeadQPS: 2800, BandwidthBitsPerSecond: 2_000_000_000, UseZeroDefaults: true, Threshold: 20, ReduceFactor: 0.9, IncreaseFactor: 1.2, MinQPS: 200, RecoveryInterval: 10 * time.Second}
	if c == nil {
		return out, nil
	}
	for _, setting := range []struct {
		value     *float64
		target    *float64
		bandwidth bool
	}{
		{c.ListQPS, &out.ListQPS, false}, {c.PutQPS, &out.PutQPS, false}, {c.HeadQPS, &out.HeadQPS, false}, {c.BandwidthBitsPerSecond, &out.BandwidthBitsPerSecond, true},
	} {
		if setting.value == nil {
			continue
		}
		v := *setting.value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nodecontrol.Options{}, errors.New("nodeControl limits must be finite and nonnegative")
		}
		if setting.bandwidth {
			if v > 1_000_000_000_000 || (v != 0 && v < 8) {
				return nodecontrol.Options{}, errors.New("nodeControl bandwidth must be zero or 8–1000000000000 bits/second")
			}
		} else if v > 1_000_000 || (v != 0 && v < 0.01) {
			return nodecontrol.Options{}, errors.New("nodeControl QPS must be zero or 0.01–1000000")
		}
		*setting.target = v
	}
	if auto := c.AutoAdjust; auto != nil {
		if auto.Consecutive503Threshold != nil {
			if *auto.Consecutive503Threshold < 1 || *auto.Consecutive503Threshold > 1_000_000 {
				return nodecontrol.Options{}, errors.New("nodeControl 503 threshold must be 1–1000000")
			}
			out.Threshold = *auto.Consecutive503Threshold
		}
		for _, setting := range []struct {
			value  *float64
			target *float64
			valid  func(float64) bool
		}{
			{auto.ReduceFactor, &out.ReduceFactor, func(v float64) bool { return v > 0 && v < 1 }},
			{auto.IncreaseFactor, &out.IncreaseFactor, func(v float64) bool { return v > 1 }},
			{auto.MinQPS, &out.MinQPS, func(v float64) bool { return v > 0 }},
		} {
			if setting.value == nil {
				continue
			}
			v := *setting.value
			if math.IsNaN(v) || math.IsInf(v, 0) || !setting.valid(v) {
				return nodecontrol.Options{}, errors.New("invalid nodeControl automatic adjustment setting")
			}
			*setting.target = v
		}
		if auto.RecoveryIntervalMS != nil {
			if *auto.RecoveryIntervalMS <= 0 || *auto.RecoveryIntervalMS > math.MaxInt64/int64(time.Millisecond) {
				return nodecontrol.Options{}, errors.New("nodeControl recoveryIntervalMs must be a positive duration")
			}
			out.RecoveryInterval = time.Duration(*auto.RecoveryIntervalMS) * time.Millisecond
		}
	}
	return out, nil
}
