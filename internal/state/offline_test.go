package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineAdoptionSnapshotsWithoutMutatingSource(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, role+".db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ClaimRole(role); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *Tx) error {
				if role == "agent" {
					if err := tx.Put("agent-identity", "self", map[string]string{"instanceId": "existing-node", "credential": "stored-secret", "enrollmentId": "original-enrollment", "masterUrl": "https://transfer.example"}); err != nil {
						return err
					}
				}
				return tx.Put("tasks", "existing", map[string]int{"id": 224})
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(dir, "backup.db")
			if err := BackupOffline(path, role, backup); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("adoption changed the source database")
			}
			db, err = Open(backup)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.View(func(tx *Tx) error {
				var task map[string]int
				found, err := tx.Get("tasks", "existing", &task)
				if err == nil && (!found || task["id"] != 224) {
					t.Fatal("snapshot lost task")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			copyBefore, _ := os.ReadFile(backup)
			if err := BackupOffline(path, role, backup); err == nil {
				t.Fatal("overwrote an existing snapshot")
			}
			copyAfter, _ := os.ReadFile(backup)
			if !bytes.Equal(copyBefore, copyAfter) {
				t.Fatal("failed backup changed an existing snapshot")
			}
		})
	}
}

func TestOfflineAdoptionRejectsUnsafeState(t *testing.T) {
	for _, scenario := range []string{"missing", "empty", "unclaimed", "wrong-role", "future-schema", "pending", "wrong-final-path", "unenrolled", "running"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path, backup := filepath.Join(dir, "source.db"), filepath.Join(dir, "backup.db")
			role := "master"
			if scenario == "unenrolled" {
				role = "agent"
			}
			if scenario == "empty" {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "missing" {
				db, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if scenario != "unclaimed" {
					if err := db.ClaimRole(role); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Update(func(tx *Tx) error {
					switch scenario {
					case "wrong-role":
						return tx.Put("metadata", "format", map[string]any{"role": "agent", "schemaVersion": 1})
					case "future-schema":
						return tx.Put("metadata", "format", map[string]any{"role": role, "schemaVersion": 2})
					case "pending":
						return tx.Put("migration", "state", map[string]any{"pending": true, "migrationId": "qa"})
					case "wrong-final-path":
						return tx.Put("migration", "state", map[string]any{"pending": false, "migrationId": "qa", "expectedDatabase": filepath.Join(dir, "final.db")})
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if scenario != "running" {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := BackupOffline(path, role, backup); err == nil {
				t.Fatal("unsafe data accepted")
			}
			if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed validation left a snapshot")
			}
			if scenario == "missing" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing source was initialized")
				}
			}
		})
	}
}
