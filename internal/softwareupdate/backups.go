package softwareupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/MoonWX/oss-transfer/internal/backupretention"
	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

// BackupController is optional so an older or unmanaged Agent can still report
// its software version without claiming support for backup maintenance.
type BackupController interface {
	BackupStats(context.Context) (BackupSummary, error)
	PruneBackups(context.Context) (BackupCleanup, error)
}

type BackupSummary struct {
	Supported        bool   `json:"supported"`
	Reason           string `json:"reason,omitempty"`
	Count            int    `json:"count"`
	Bytes            int64  `json:"bytes"`
	ReclaimableCount int    `json:"reclaimableCount"`
	ReclaimableBytes int64  `json:"reclaimableBytes"`
}

type BackupCleanup struct {
	RemovedCount int   `json:"removedCount"`
	FreedBytes   int64 `json:"freedBytes"`
}

func (m *Manager) backupManager() (*backupretention.Manager, error) {
	if !m.info.Supported {
		return nil, errors.New("Backup maintenance requires a managed native installation")
	}
	installed, err := inspectInstallation(m.options, m.executable)
	if err != nil {
		return nil, err
	}
	return backupretention.New(installed.BackupDir, backupretention.Identity{
		Role: m.options.Role, ServiceName: m.options.ServiceName,
		Config: m.options.ConfigPath, DataDir: m.options.DataDirectory,
	})
}

func (m *Manager) BackupStats(ctx context.Context) (BackupSummary, error) {
	if err := ctx.Err(); err != nil {
		return BackupSummary{}, err
	}
	if !m.info.Supported {
		return BackupSummary{Reason: "UNMANAGED"}, nil
	}
	manager, err := m.backupManager()
	if err != nil {
		return BackupSummary{}, err
	}
	listing, err := manager.List()
	if err != nil {
		return BackupSummary{}, err
	}
	result := BackupSummary{Supported: true, ReclaimableBytes: listing.ReclaimableBytes}
	for _, entry := range listing.Entries {
		if entry.Reason == "OTHER_INSTALLATION" {
			continue
		}
		result.Count++
		result.Bytes += entry.SizeBytes
		if entry.Eligible {
			result.ReclaimableCount++
		}
	}
	return result, nil
}

func (m *Manager) PruneBackups(ctx context.Context) (BackupCleanup, error) {
	// Serialize with starting an update, then check the persisted helper status.
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return BackupCleanup{}, err
	}
	if active(m.Status().Phase) {
		return BackupCleanup{}, backupretention.ErrBusy
	}
	manager, err := m.backupManager()
	if err != nil {
		return BackupCleanup{}, err
	}
	result, err := manager.Prune(backupretention.Options{})
	return BackupCleanup{RemovedCount: len(result.Removed), FreedBytes: result.FreedBytes}, err
}

// RunBackupMaintenance is used by the candidate binary during installation.
// It never opens the live task database or starts the service.
func RunBackupMaintenance(role, configPath, root, action, entry string, out io.Writer) error {
	if configPath == "" || root == "" {
		return errors.New("backup maintenance requires --config and --backup-dir")
	}
	if action != "complete" && entry != "" {
		return errors.New("--backup-entry requires complete maintenance")
	}
	config, err := servicehost.LoadConfig(configPath, role)
	if err != nil {
		return err
	}
	manager, err := backupretention.New(root, backupretention.Identity{Role: role, ServiceName: config.ServiceName, Config: configPath, DataDir: config.DataDirectory})
	if err != nil {
		return err
	}
	var result any
	switch action {
	case "list":
		result, err = manager.List()
	case "prune":
		var cleaned backupretention.Result
		cleaned, err = manager.Prune(backupretention.Options{})
		result = map[string]any{"count": len(cleaned.Removed), "removed": cleaned.Removed, "freedBytes": cleaned.FreedBytes}
	case "complete":
		err = manager.Complete(entry, "")
		result = map[string]string{"completed": entry}
	default:
		return fmt.Errorf("unknown backup maintenance action %q", action)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
