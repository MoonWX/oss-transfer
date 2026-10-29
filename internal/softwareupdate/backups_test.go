package softwareupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/backupretention"
)

func TestBackupMaintenanceUsesInstallationAndProtectsUpgrade(t *testing.T) {
	options, executable := managedFixture(t)
	root := filepath.Join(filepath.Dir(options.DataDirectory), "backups")
	metadata := filepath.Join(filepath.Dir(executable), "install.tsv")
	if runtime.GOOS == "windows" {
		metadata = filepath.Join(filepath.Dir(executable), "install.json")
	}
	raw, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		var installed installation
		if err = json.Unmarshal(raw, &installed); err != nil {
			t.Fatal(err)
		}
		installed.BackupDir = root
		raw, err = json.Marshal(installed)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		raw = append(raw, []byte("backupDir\t"+root+"\n")...)
	}
	if err = os.WriteFile(metadata, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{options: options, executable: executable, info: Info{Supported: true}}
	ctx := context.Background()
	stats, err := m.BackupStats(ctx)
	if err != nil || !stats.Supported || stats.Count != 0 {
		t.Fatal(stats, err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("listing created backup directory", err)
	}
	config, _ := os.ReadFile(options.ConfigPath)
	for _, name := range []string{"backup-1", "backup-2", "backup-3"} {
		dir := filepath.Join(root, name)
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{filepath.Base(metadata): raw, "service.json": config, "binary" + executableSuffix(): []byte("saved executable"), "database": []byte("saved state")}
		for file, content := range files {
			if err = os.WriteFile(filepath.Join(dir, file), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err = RunBackupMaintenance("master", options.ConfigPath, root, "complete", name, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(root, "other-installation")
	if err = os.CopyFS(foreign, os.DirFS(filepath.Join(root, "backup-3"))); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(foreign, backupretention.MarkerName)
	markerRaw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err = json.Unmarshal(markerRaw, &marker); err != nil {
		t.Fatal(err)
	}
	marker["identity"].(map[string]any)["serviceName"] = "other-installation"
	markerRaw, err = json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(markerPath, markerRaw, 0600); err != nil {
		t.Fatal(err)
	}
	stats, err = m.BackupStats(ctx)
	if err != nil || stats.Count != 3 || stats.ReclaimableCount != 1 || stats.ReclaimableBytes == 0 {
		t.Fatal(stats, err)
	}
	if err = writeJSON(m.statusPath(), Status{Phase: "APPLYING", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.PruneBackups(ctx); !errors.Is(err, backupretention.ErrBusy) {
		t.Fatal("cleanup allowed during update", err)
	}
	if _, err = os.Stat(filepath.Join(root, "backup-1")); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(m.statusPath(), Status{Phase: "COMPLETED"}); err != nil {
		t.Fatal(err)
	}
	result, err := m.PruneBackups(ctx)
	if err != nil || result.RemovedCount != 1 || result.FreedBytes == 0 {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(root, "backup-1")); !os.IsNotExist(err) {
		t.Fatal("old backup remains", err)
	}
	if _, err = os.Stat(filepath.Join(root, "backup-3")); err != nil {
		t.Fatal("latest backup lost", err)
	}
	// Changing the service identity cannot make the cleanup act on another install.
	m.options.ServiceName = "another-service"
	if _, err = m.PruneBackups(ctx); err == nil {
		t.Fatal("changed identity accepted")
	}
}

func TestBackupMaintenanceRejectsTraversalAndMixedArguments(t *testing.T) {
	options, _ := managedFixture(t)
	root := filepath.Join(filepath.Dir(options.DataDirectory), "backups")
	for _, input := range [][2]string{{"complete", "../data"}, {"prune", "backup-1"}, {"delete-everything", ""}} {
		if err := RunBackupMaintenance("master", options.ConfigPath, root, input[0], input[1], io.Discard); err == nil {
			t.Fatal(input)
		}
	}
}
