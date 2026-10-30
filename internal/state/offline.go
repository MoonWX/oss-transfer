package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// BackupOffline verifies an existing database without initializing or modifying
// it. A running service holds the exclusive bbolt lock, so adoption fails until
// it has stopped. The destination must be new and in a private directory.
func BackupOffline(path, role, destination string) error {
	return BackupOfflineWithProgress(path, role, destination, nil)
}

// BackupOfflineWithProgress reports the actual snapshot copy and disk flush.
// The progress stream never contains database contents or stored credentials.
func BackupOfflineWithProgress(path, role, destination string, progress io.Writer) error {
	if role != "master" && role != "agent" {
		return errors.New("invalid database role")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("existing database must be a nonempty regular file")
	}
	db, err := openExistingOffline(path, info, true)
	if err != nil {
		return fmt.Errorf("open offline database (stop its service first): %w", err)
	}
	defer db.Close()
	return db.View(func(raw *bolt.Tx) error {
		if err := validateOfflineTransaction(raw, role, path); err != nil {
			return err
		}
		credentials, err := offlineCredentialCipher(raw, path)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(CredentialKeyPath(destination)); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return errors.New("backup key already exists")
		}
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		complete, keyWritten := false, false
		defer func() {
			_ = file.Close()
			if !complete {
				_ = os.Remove(destination)
				if keyWritten {
					_ = os.Remove(CredentialKeyPath(destination))
				}
			}
		}()
		if credentials != nil {
			if err := writeCredentialKey(CredentialKeyPath(destination), credentials.key); err != nil {
				return err
			}
			keyWritten = true
		}
		monitor := newBackupProgress(progress, raw.Size(), 10*time.Second)
		defer monitor.stop()
		if _, err := raw.WriteTo(backupWriter{file, monitor}); err != nil {
			return err
		}
		monitor.syncing.Store(true)
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		complete = true
		monitor.stop()
		if progress != nil {
			fmt.Fprintf(progress, "Database backup complete: %s (%s).\n", backupSize(raw.Size()), time.Since(monitor.started).Round(time.Second))
		}
		return nil
	})
}

type backupProgress struct {
	started  time.Time
	copied   atomic.Int64
	syncing  atomic.Bool
	done     chan struct{}
	finished chan struct{}
}

func newBackupProgress(output io.Writer, size int64, interval time.Duration) *backupProgress {
	p := &backupProgress{started: time.Now()}
	if output == nil {
		return p
	}
	fmt.Fprintf(output, "Backing up database: %s.\n", backupSize(size))
	p.done, p.finished = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				elapsed := time.Since(p.started).Round(time.Second)
				if p.syncing.Load() {
					fmt.Fprintf(output, "Database backup: syncing to disk (%s elapsed).\n", elapsed)
				} else {
					copied := p.copied.Load()
					fmt.Fprintf(output, "Database backup: %s / %s (%.0f%%; %s elapsed).\n", backupSize(copied), backupSize(size), 100*float64(copied)/float64(size), elapsed)
				}
			}
		}
	}()
	return p
}

func (p *backupProgress) stop() {
	if p.done != nil {
		close(p.done)
		<-p.finished
		p.done = nil
	}
}

type backupWriter struct {
	io.Writer
	progress *backupProgress
}

func (w backupWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.progress.copied.Add(int64(n))
	return n, err
}

func backupSize(size int64) string {
	if size >= 1<<30 {
		return fmt.Sprintf("%.2f GiB", float64(size)/(1<<30))
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
}

func validateOfflineTransaction(raw *bolt.Tx, role, path string) error {
	tx := &Tx{tx: raw}
	var metadata databaseFormat
	found, err := tx.Get("metadata", "format", &metadata)
	if err != nil {
		return err
	}
	if !found || metadata.Role != role || !supportedSchema(metadata.SchemaVersion) {
		return errors.New("existing database has no matching role or supported schema")
	}
	var migration struct {
		Pending          *bool  `json:"pending"`
		MigrationID      string `json:"migrationId"`
		ExpectedDatabase string `json:"expectedDatabase,omitempty"`
	}
	found, err = tx.Get("migration", "state", &migration)
	if err != nil {
		return err
	}
	if found && (migration.Pending == nil || *migration.Pending || migration.MigrationID == "" || migration.ExpectedDatabase != "" && filepath.Clean(migration.ExpectedDatabase) != filepath.Clean(path)) {
		return ErrMigrationPending
	}
	if role == "agent" {
		var identity struct {
			InstanceID   string `json:"instanceId"`
			Credential   string `json:"credential"`
			EnrollmentID string `json:"enrollmentId"`
			MasterURL    string `json:"masterUrl"`
		}
		found, err = tx.Get("agent-identity", "self", &identity)
		if err != nil {
			return err
		}
		if !found || identity.InstanceID == "" || identity.Credential == "" || identity.EnrollmentID == "" || identity.MasterURL == "" {
			return errors.New("existing Agent must have a completed enrollment")
		}
	}
	return nil
}
