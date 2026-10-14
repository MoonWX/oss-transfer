package legacyimport

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

// These are the resolved V4 properties consumed by the shared node controller.
// SDK retry policy, process concurrency and multipart settings are separate.
var effectiveNodeKeys = []string{
	"rate-limit.src.qps.list", "rate-limit.dest.qps.put", "rate-limit.dest.qps.head", "rate-limit.bandwidth",
	"rate-limit.auto-adjust.consecutive-503-threshold", "rate-limit.auto-adjust.reduce-factor", "rate-limit.auto-adjust.increase-factor",
	"rate-limit.auto-adjust.min-put-qps", "rate-limit.auto-adjust.interval-ms",
}

// MapEffectiveNodeControl accepts a complete, explicitly resolved setting set.
// It never reads the environment, evaluates placeholders or guesses old defaults.
func MapEffectiveNodeControl(values map[string]string) (*servicehost.NodeControlConfig, error) {
	known := map[string]bool{}
	for _, name := range effectiveNodeKeys {
		known[name] = true
		value, found := values[name]
		if !found || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "${}") {
			return nil, fmt.Errorf("resolved effective setting required: %s", name)
		}
	}
	for name := range values {
		if !known[name] {
			return nil, fmt.Errorf("unsupported node-control setting: %s", name)
		}
	}
	floats := map[string]*float64{}
	for _, name := range []string{effectiveNodeKeys[0], effectiveNodeKeys[1], effectiveNodeKeys[2], effectiveNodeKeys[3], effectiveNodeKeys[5], effectiveNodeKeys[6], effectiveNodeKeys[7]} {
		value, err := strconv.ParseFloat(strings.TrimSpace(values[name]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric effective setting: %s", name)
		}
		floats[name] = &value
	}
	threshold, err := strconv.Atoi(strings.TrimSpace(values[effectiveNodeKeys[4]]))
	if err != nil {
		return nil, errors.New("invalid consecutive 503 threshold")
	}
	interval, err := strconv.ParseInt(strings.TrimSpace(values[effectiveNodeKeys[8]]), 10, 64)
	if err != nil {
		return nil, errors.New("invalid automatic recovery interval")
	}
	config := &servicehost.NodeControlConfig{ListQPS: floats[effectiveNodeKeys[0]], PutQPS: floats[effectiveNodeKeys[1]], HeadQPS: floats[effectiveNodeKeys[2]], BandwidthBitsPerSecond: floats[effectiveNodeKeys[3]], AutoAdjust: &servicehost.AutoAdjustConfig{Consecutive503Threshold: &threshold, ReduceFactor: floats[effectiveNodeKeys[5]], IncreaseFactor: floats[effectiveNodeKeys[6]], MinQPS: floats[effectiveNodeKeys[7]], RecoveryIntervalMS: &interval}}
	if _, err := config.Options(); err != nil {
		return nil, err
	}
	return config, nil
}

// WriteRuntimeConfig creates the eventual installation's configuration files in
// the private staging area. Settings must cover every staged Agent; they are not
// inferred from an application.yml that could have environment overrides.
// Finalize separately verifies task/history, recovery and runtime acceptance.
func (p *Prepared) WriteRuntimeConfig(ctx context.Context, masterListen string, effective map[string]map[string]string) error {
	if p.closed {
		return errors.New("migration preparation is closed")
	}
	if len(effective) != len(p.Manifest.Nodes) {
		return errors.New("effective node settings must cover every staged Agent")
	}
	configs := map[string]servicehost.Config{}
	masterData := filepath.Join(p.Manifest.Destination, "master")
	configs["master/service.json"] = servicehost.Config{ServiceName: "oss-transfer-master", DataDirectory: masterData, Listen: masterListen, ReadyFile: filepath.Join(masterData, "runtime", "ready.json"), LogFile: filepath.Join(masterData, "logs", "service.log")}
	if err := configs["master/service.json"].Validate("master"); err != nil {
		return err
	}
	for _, node := range p.Manifest.Nodes {
		settings, found := effective[node.LegacyNode]
		if !found {
			return fmt.Errorf("effective node settings are missing: %s", node.LegacyNode)
		}
		control, err := MapEffectiveNodeControl(settings)
		if err != nil {
			return err
		}
		dir := filepath.Dir(filepath.FromSlash(node.Database))
		data := filepath.Join(p.Manifest.Destination, dir)
		config := servicehost.Config{ServiceName: "oss-transfer-agent-" + node.InstanceID, DataDirectory: data, ReadyFile: filepath.Join(data, "runtime", "ready.json"), LogFile: filepath.Join(data, "logs", "service.log"), NodeControl: control}
		if err := config.Validate("agent"); err != nil {
			return err
		}
		configs[filepath.Join(dir, "service.json")] = config
	}
	// Validate everything before writing any runtime file. Partial failures stay
	// behind the durable migration fence; files are never overwritten on retry.
	for relative, config := range configs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writePrivateJSON(filepath.Join(p.Directory, relative), config); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(filepath.Join(p.Directory, relative))); err != nil {
			return err
		}
	}
	return nil
}
