package servicehost_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

func writeServiceJSON(t *testing.T, data, extra string) string {
	t.Helper()
	ready, log := filepath.Join(data, "runtime", "ready.json"), filepath.Join(data, "logs", "service.log")
	document := `{"serviceName":"agent","dataDirectory":` + quote(data) + `,"readyFile":` + quote(ready) + `,"logFile":` + quote(log) + extra + `}`
	file := filepath.Join(t.TempDir(), "service.json")
	if err := os.WriteFile(file, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}

func TestComparisonConfigLimits(t *testing.T) {
	data := t.TempDir()
	for _, valid := range []string{
		``,
		`,"comparison":{}`,
		`,"comparison":{"budgetBytes":0,"retentionDays":0,"minimumFreeBytes":0}`,
		`,"comparison":{"budgetBytes":67108864,"retentionDays":3650,"minimumFreeBytes":1}`,
		`,"comparison":{"budgetBytes":1099511627776,"retentionDays":1}`,
	} {
		if _, err := servicehost.LoadConfig(writeServiceJSON(t, data, valid), "agent"); err != nil {
			t.Fatalf("rejected %s: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		`,"comparison":{"budgetBytes":67108863}`,
		`,"comparison":{"budgetBytes":1}`,
		`,"comparison":{"budgetBytes":-1}`,
		`,"comparison":{"retentionDays":-1}`,
		`,"comparison":{"retentionDays":3651}`,
		`,"comparison":{"minimumFreeBytes":-1}`,
		`,"comparison":{"maxRetained":10}`,
		`,"comparison":{"budgetBytes":"2GiB"}`,
		`,"comparison":[]`,
	} {
		if _, err := servicehost.LoadConfig(writeServiceJSON(t, data, invalid), "agent"); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	master := servicehost.Config{ServiceName: "master", DataDirectory: data, Listen: "127.0.0.1:9000", ReadyFile: filepath.Join(data, "runtime", "ready.json"), LogFile: filepath.Join(data, "logs", "service.log"), Comparison: &servicehost.ComparisonConfig{}}
	if err := master.Validate("master"); err == nil || !strings.Contains(err.Error(), "Agent") {
		t.Fatalf("Master accepted comparison settings: %v", err)
	}
}

func TestComparisonConfigReachesAgentOptions(t *testing.T) {
	data := t.TempDir()
	config, err := servicehost.LoadConfig(writeServiceJSON(t, data, `,"comparison":{"budgetBytes":134217728,"retentionDays":7,"minimumFreeBytes":536870912}`), "agent")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(data, "comparisons")
	got := config.Comparison.Options(directory)
	if got.Directory != directory || got.BudgetBytes != 128<<20 || got.RetentionDays != 7 || got.MinimumFreeBytes != 512<<20 {
		t.Fatalf("settings lost: %+v", got)
	}
	// Omitted settings keep the comparison defaults (zero values).
	config, err = servicehost.LoadConfig(writeServiceJSON(t, data, ``), "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Comparison.Options(directory); got.Directory != directory || got.BudgetBytes != 0 || got.RetentionDays != 0 || got.MinimumFreeBytes != 0 {
		t.Fatalf("defaults changed: %+v", got)
	}
}
