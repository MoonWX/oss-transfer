package master

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

// noWriteProbeAgent is an Agent from before the write probe.
type noWriteProbeAgent struct{ *agentservice.Service }

func (a noWriteProbeAgent) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	if command.Operation == "local_write_probe" {
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
	return a.Service.Execute(ctx, command)
}

func TestDownloadPrecheckProvesTheAgentCanWrite(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current := connectTestAgent(t, f, 1, func(s *agentservice.Service) agenttransport.Executor { return s })
	older := connectTestAgent(t, f, 2, func(s *agentservice.Service) agenttransport.Executor { return noWriteProbeAgent{s} })
	validate := func(node, path string) map[string]any {
		t.Helper()
		row := taskCreateRequest{Direction: "OSS_TO_LOCAL",
			SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
			DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node, path}}}}
		code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
		if code != 200 {
			t.Fatal(code, response)
		}
		bindings := response["data"].(map[string]any)["nodeBindings"].([]any)
		return bindings[0].(map[string]any)
	}
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "new", "download")} {
		check := validate(current.Address, path)
		if check["valid"] != true || check["writeProbeDirectory"] != root || check["freeBytes"] == nil || !strings.Contains(check["message"].(string), "可用空间") {
			t.Fatalf("writable destination %s: %+v", path, check)
		}
		if runtime.GOOS != "windows" && check["effectiveUid"] != float64(os.Geteuid()) {
			t.Fatalf("effective UID missing: %+v", check)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("precheck left files or created directories: %v %v", entries, err)
	}
	if check := validate(older.Address, root); check["valid"] != true || check["freeBytes"] != nil {
		t.Fatalf("an Agent without the probe must keep the previous precheck: %+v", check)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	for _, path := range []string{locked, filepath.Join(locked, "download")} {
		check := validate(current.Address, path)
		message, _ := check["message"].(string)
		if check["valid"] != false || check["code"] != "LOCAL_PATH_NOT_WRITABLE" || !strings.Contains(message, "UID "+strconv.Itoa(os.Geteuid())) || !strings.Contains(message, locked) {
			t.Fatalf("unwritable destination %s: %+v", path, check)
		}
	}
}
