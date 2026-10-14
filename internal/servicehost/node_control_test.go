package servicehost

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func pointer[T any](value T) *T { return &value }

func TestNodeControlConfigurationDefaultsZeroAndCustomValues(t *testing.T) {
	defaults, err := (*NodeControlConfig)(nil).Options()
	if err != nil || defaults.ListQPS != 100 || defaults.PutQPS != 1400 || defaults.HeadQPS != 2800 || defaults.BandwidthBitsPerSecond != 2_000_000_000 || defaults.RecoveryInterval != 10*time.Second {
		t.Fatalf("defaults changed: %+v %v", defaults, err)
	}
	config := &NodeControlConfig{ListQPS: pointer(0.0), BandwidthBitsPerSecond: pointer(0.0), AutoAdjust: &AutoAdjustConfig{Consecutive503Threshold: pointer(12), ReduceFactor: pointer(0.75), IncreaseFactor: pointer(1.1), MinQPS: pointer(20.0), RecoveryIntervalMS: pointer(int64(2500))}}
	got, err := config.Options()
	if err != nil || got.ListQPS != 0 || got.BandwidthBitsPerSecond != 0 || got.PutQPS != 1400 || !got.UseZeroDefaults || got.Threshold != 12 || got.ReduceFactor != 0.75 || got.RecoveryInterval != 2500*time.Millisecond {
		t.Fatalf("zero/default distinction lost: %+v %v", got, err)
	}
}

func TestNodeControlConfigCheckedBeforeStarting(t *testing.T) {
	data := t.TempDir()
	base := Config{ServiceName: "agent", DataDirectory: data, ReadyFile: filepath.Join(data, "runtime", "ready.json"), LogFile: filepath.Join(data, "logs", "service.log")}
	for _, bad := range []*NodeControlConfig{
		{ListQPS: pointer(-1.0)}, {HeadQPS: pointer(0.001)}, {PutQPS: pointer(1_000_001.0)}, {BandwidthBitsPerSecond: pointer(7.0)},
		{AutoAdjust: &AutoAdjustConfig{Consecutive503Threshold: pointer(0)}}, {AutoAdjust: &AutoAdjustConfig{ReduceFactor: pointer(1.0)}},
		{AutoAdjust: &AutoAdjustConfig{IncreaseFactor: pointer(1.0)}}, {AutoAdjust: &AutoAdjustConfig{MinQPS: pointer(0.0)}},
		{AutoAdjust: &AutoAdjustConfig{RecoveryIntervalMS: pointer(int64(0))}}, {AutoAdjust: &AutoAdjustConfig{RecoveryIntervalMS: pointer(int64(math.MaxInt64))}},
	} {
		base.NodeControl = bad
		encoded, _ := json.Marshal(base)
		file := filepath.Join(data, "service.json")
		if err := os.WriteFile(file, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(file, "agent"); err == nil {
			t.Fatalf("invalid settings passed check-config path: %+v", bad)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := (&NodeControlConfig{ListQPS: &value}).Options(); err == nil {
			t.Fatal("nonfinite setting accepted")
		}
	}
	base.NodeControl = &NodeControlConfig{}
	if err := base.Validate("master"); err == nil {
		t.Fatal("Master accepted unused Agent settings")
	}
}

func TestConfiguredNodeLimitsReachActualAgentController(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config := &NodeControlConfig{ListQPS: pointer(0.0), PutQPS: pointer(123.0), HeadQPS: pointer(456.0), BandwidthBitsPerSecond: pointer(80_000_000.0), AutoAdjust: &AutoAdjustConfig{Consecutive503Threshold: pointer(7)}}
	options, err := config.Options()
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{NodeControl: options, SpoolDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := service.Nodes.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]float64{"list": 0, "put": 123, "head": 456, "bandwidth": 80_000_000} {
		rate := snapshot[kind].(map[string]any)
		field := "configuredLimitQps"
		if kind == "bandwidth" {
			field = "configuredLimitBps"
		}
		if rate[field] != want {
			t.Fatalf("%s configuration was ignored: %#v", kind, rate)
		}
	}
}
