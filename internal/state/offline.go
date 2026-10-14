package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

// BackupOffline verifies an existing database without initializing or modifying
// it. A running service holds the exclusive bbolt lock, so adoption fails until
// it has stopped. The destination must be new and in a private directory.
func BackupOffline(path, role, destination string) error {
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
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		complete := false
		defer func() {
			_ = file.Close()
			if !complete {
				_ = os.Remove(destination)
			}
		}()
		if _, err := raw.WriteTo(file); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		complete = true
		return nil
	})
}

func validateOfflineTransaction(raw *bolt.Tx, role, path string) error {
	tx := &Tx{tx: raw}
	var metadata struct {
		Role          string `json:"role"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	found, err := tx.Get("metadata", "format", &metadata)
	if err != nil {
		return err
	}
	if !found || metadata.Role != role || metadata.SchemaVersion != 1 {
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
