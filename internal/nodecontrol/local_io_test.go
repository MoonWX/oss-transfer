package nodecontrol

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// Local storage limits are unlimited unless configured or saved, and never
// delay a caller then.
func TestLocalLimitsDefaultToUnlimited(t *testing.T) {
	f := newFixture(t, Options{UseZeroDefaults: true})
	for _, kind := range []string{"localOps", "localBandwidth"} {
		if state := rate(t, f.controller, kind)["state"]; state != "UNLIMITED" {
			t.Fatalf("%s state %v", kind, state)
		}
	}
	ctx := context.Background()
	for range 1000 {
		if err := f.controller.WaitLocalOperation(ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.controller.WaitLocalBytes(ctx, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
}

// A saved local limit survives a restart, is kept apart from the request
// limits an older Agent validates, and local bandwidth is whole bytes.
func TestLocalLimitsAreSavedWhereOlderAgentsIgnoreThem(t *testing.T) {
	f := newFixture(t, Options{UseZeroDefaults: true})
	ctx := context.Background()
	if err := f.controller.SetRate(ctx, "localBandwidth", 1_000_000_004); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.SetRate(ctx, "localOps", 250); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.SetRate(ctx, "localOps", 0.001); err == nil {
		t.Fatal("a local operation limit below 0.01/s was accepted")
	}
	f.restart(t, func(*Options) {})
	bandwidth, ops := rate(t, f.controller, "localBandwidth"), rate(t, f.controller, "localOps")
	if bandwidth["limitBps"] != float64(1_000_000_000) || bandwidth["limitSource"] != "PERSISTED_MANUAL" || ops["limitQps"] != float64(250) {
		t.Fatalf("after restart: bandwidth %v, operations %v", bandwidth, ops)
	}
	var saved map[string]json.RawMessage
	if err := f.db.View(func(tx *state.Tx) error {
		_, err := tx.Get("node-control", "settings", &saved)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var overrides, local map[string]float64
	_ = json.Unmarshal(saved["overrides"], &overrides)
	_ = json.Unmarshal(saved["localOverrides"], &local)
	if _, found := overrides["localOps"]; found || local["localOps"] != 250 || local["localBandwidth"] != 1_000_000_000 {
		t.Fatalf("overrides %v, local overrides %v", overrides, local)
	}
}

// A caller over the local budget waits, without holding a reservation, until
// the limit allows it; lifting the limit releases it at once.
func TestLocalLimitsHoldCallersUntilTheBudgetAllows(t *testing.T) {
	f := newFixture(t, Options{UseZeroDefaults: true})
	ctx := context.Background()
	if err := f.controller.SetRate(ctx, "localBandwidth", 8); err != nil { // 1 byte per second
		t.Fatal(err)
	}
	if err := f.controller.SetRate(ctx, "localOps", 0.01); err != nil {
		t.Fatal(err)
	}
	// The short credit (at least 64 KiB) and the first operation pass at once.
	if err := f.controller.WaitLocalBytes(ctx, localCreditMin); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.WaitLocalOperation(ctx); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 2)
	go func() { released <- f.controller.WaitLocalBytes(ctx, 1) }()
	go func() { released <- f.controller.WaitLocalOperation(ctx) }()
	select {
	case err := <-released:
		t.Fatalf("a caller over the local budget was admitted: %v", err)
	default:
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.controller.WaitLocalBytes(cancelled, 1); err == nil {
		t.Fatal("a cancelled caller was admitted")
	}
	if err := f.controller.SetRate(ctx, "localBandwidth", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.SetRate(ctx, "localOps", 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-released; err != nil {
			t.Fatal(err)
		}
	}
}
