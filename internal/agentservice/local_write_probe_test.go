package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

func writeProbe(t *testing.T, path string) WriteProbe {
	t.Helper()
	payload, _ := json.Marshal(PathRequest{Path: path})
	response, err := (&Service{}).Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "local_write_probe", Payload: payload}})
	var probe WriteProbe
	if err != nil || response.Status != 200 || json.Unmarshal(response.Payload, &probe) != nil {
		t.Fatalf("local_write_probe: %+v %v", response, err)
	}
	return probe
}

func TestWriteProbeCreatesAndRemovesAFileInTheNearestExistingDirectory(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "missing", "nested")} {
		probe := writeProbe(t, path)
		if !probe.Writable || probe.ProbeDirectory != root || probe.ErrorCode != "" || probe.Path != path {
			t.Fatalf("probe of %s: %+v", path, probe)
		}
		if runtime.GOOS != "windows" && (probe.EffectiveUID == nil || *probe.EffectiveUID != os.Geteuid()) {
			t.Fatalf("effective UID not reported: %+v", probe)
		}
		if (runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" || runtime.GOOS == "windows") && (probe.FreeBytes == nil || probe.TotalBytes == nil || *probe.FreeBytes < 0 || *probe.TotalBytes < *probe.FreeBytes) {
			t.Fatalf("free space not reported: %+v", probe)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("probe left files or created directories: %v %v", entries, err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if probe := writeProbe(t, filepath.Join(file, "below")); probe.Writable || probe.ErrorCode != WriteProbeNotDirectory {
		t.Fatalf("probe below a file: %+v", probe)
	}
}

func TestWriteProbeReportsAPermissionDeniedDestination(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict this user")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	for _, path := range []string{locked, filepath.Join(locked, "download")} {
		probe := writeProbe(t, path)
		if probe.Writable || probe.ErrorCode != WriteProbePermissionDenied || probe.ProbeDirectory != locked || probe.EffectiveUID == nil || *probe.EffectiveUID != os.Geteuid() || probe.Error == "" {
			t.Fatalf("probe of %s: %+v", path, probe)
		}
	}
	if _, err := os.Stat(filepath.Join(locked, "download")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe created the destination", err)
	}
}
