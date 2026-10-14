package legacyimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

func effectiveSettings() map[string]string {
	return map[string]string{
		"rate-limit.src.qps.list": "0", "rate-limit.dest.qps.put": "123", "rate-limit.dest.qps.head": "456", "rate-limit.bandwidth": "80000000",
		"rate-limit.auto-adjust.consecutive-503-threshold": "7", "rate-limit.auto-adjust.reduce-factor": "0.8", "rate-limit.auto-adjust.increase-factor": "1.1",
		"rate-limit.auto-adjust.min-put-qps": "25", "rate-limit.auto-adjust.interval-ms": "2500",
	}
}

func TestMapEffectiveNodeControlRejectsUnresolvedAndUnsupportedSettings(t *testing.T) {
	config, err := MapEffectiveNodeControl(effectiveSettings())
	if err != nil {
		t.Fatal(err)
	}
	options, err := config.Options()
	if err != nil || options.ListQPS != 0 || options.PutQPS != 123 || options.HeadQPS != 456 || options.BandwidthBitsPerSecond != 80_000_000 || options.Threshold != 7 || options.UseZeroDefaults != true {
		t.Fatalf("effective values changed: %+v %v", options, err)
	}
	for _, change := range []func(map[string]string){
		func(values map[string]string) { delete(values, "rate-limit.bandwidth") },
		func(values map[string]string) { values["rate-limit.bandwidth"] = "${BANDWIDTH:80000000}" },
		func(values map[string]string) { values["rate-limit.auto-adjust.retry-delay-ms"] = "500" },
		func(values map[string]string) { values["rate-limit.src.qps.list"] = "NaN" },
		func(values map[string]string) { values["rate-limit.auto-adjust.consecutive-503-threshold"] = "7.5" },
		func(values map[string]string) { values["rate-limit.bandwidth"] = "-1" },
	} {
		settings := effectiveSettings()
		change(settings)
		if _, err := MapEffectiveNodeControl(settings); err == nil {
			t.Fatal("unresolved/unsupported settings accepted")
		}
	}
}

func TestWriteRuntimeConfigsPreservesPortEffectiveRatesAndAgentRoles(t *testing.T) {
	f := prepareFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	settings := map[string]map[string]string{}
	for _, node := range p.Manifest.Nodes {
		settings[node.LegacyNode] = effectiveSettings()
	}
	if err := p.WriteRuntimeConfig(context.Background(), "0.0.0.0:2185", settings); err != nil {
		t.Fatal(err)
	}
	master, err := servicehost.LoadConfig(filepath.Join(p.Directory, "master", "service.json"), "master")
	if err != nil || master.Listen != "0.0.0.0:2185" || master.DataDirectory != filepath.Join(p.Manifest.Destination, "master") {
		t.Fatalf("master deployment changed: %+v %v", master, err)
	}
	for _, node := range p.Manifest.Nodes {
		path := filepath.Join(p.Directory, filepath.Dir(filepath.FromSlash(node.Database)), "service.json")
		config, err := servicehost.LoadConfig(path, "agent")
		if err != nil {
			t.Fatal(err)
		}
		limits, err := config.NodeControl.Options()
		if err != nil || limits.ListQPS != 0 || limits.BandwidthBitsPerSecond != 80_000_000 || limits.Threshold != 7 {
			t.Fatalf("Agent settings changed: %+v %v", limits, err)
		}
		if config.Listen != "" || !strings.HasPrefix(config.DataDirectory, p.Manifest.Destination+string(os.PathSeparator)) {
			t.Fatal("Agent config points to temporary/source directory")
		}
	}
	if err := p.WriteRuntimeConfig(context.Background(), "0.0.0.0:9999", settings); err == nil {
		t.Fatal("existing runtime configuration overwritten")
	}
}

func TestWriteRuntimeConfigsFailsBeforeWritingOnMissingEffectiveSettings(t *testing.T) {
	f := prepareFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	settings := map[string]map[string]string{"master": effectiveSettings(), "node": effectiveSettings()}
	if err := p.WriteRuntimeConfig(context.Background(), ":1685", settings); err == nil {
		t.Fatal("missing worker effective settings accepted")
	}
	if _, err := os.Stat(filepath.Join(p.Directory, "master", "service.json")); !os.IsNotExist(err) {
		t.Fatal("partial config was written before validation")
	}
}
