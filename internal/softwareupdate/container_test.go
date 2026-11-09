package softwareupdate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withContainerMarker(t *testing.T, present bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if present {
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	previous := containerMarkers
	containerMarkers = []string{marker}
	t.Cleanup(func() { containerMarkers = previous })
}

// A native service installed by the installer inside a systemd container
// (with /.dockerenv) is a native installation, not a Compose one.
func TestInstallerMetadataOutranksContainerMarker(t *testing.T) {
	options, executable := managedFixture(t)
	withContainerMarker(t, true)
	if info := evaluate(options, executable); info.Reason == "CONTAINER" {
		t.Fatal("native installation was reported as a container", info)
	}
	if err := checkHost(); err != nil && err.Error() == ContainerUpdateMessage {
		t.Fatal("update helper refused a native installation because of a container marker")
	}
	unmanaged := filepath.Join(t.TempDir(), "oss-transfer-master"+executableSuffix())
	if err := os.WriteFile(unmanaged, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if info := evaluate(options, unmanaged); info.Supported || info.Reason != "CONTAINER" || info.Detail != ContainerUpdateMessage {
		t.Fatal("container without installer metadata was not identified", info)
	}
	withContainerMarker(t, false)
	if info := evaluate(options, unmanaged); info.Reason != "UNMANAGED" {
		t.Fatal("unmanaged binary was reported with the wrong reason", info)
	}
}

// Update support is re-read periodically and before every update, so fixing
// the host does not require a service restart.
func TestUpdateSupportIsReevaluated(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	supported := false
	probes := 0
	m := &Manager{now: func() time.Time { return now }, probe: func() Info {
		probes++
		if supported {
			return Info{Supported: true}
		}
		return Info{Reason: "CONTAINER", Detail: ContainerUpdateMessage}
	}}
	m.info, m.probedAt = m.probe(), now
	supported = true
	if m.Info().Supported || probes != 1 {
		t.Fatal("Info re-read the installation on every call")
	}
	now = now.Add(infoRefresh)
	if !m.Info().Supported || probes != 2 {
		t.Fatal("Info did not refresh after the interval")
	}
	supported = false
	if _, err := m.Start(context.Background(), "9.9.9", false); err == nil || err.Error() != ContainerUpdateMessage || probes != 3 {
		t.Fatal("Start did not re-evaluate update support", err, probes)
	}
}
