package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func restoreFixture(t *testing.T, role string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path, backup := filepath.Join(dir, "live.db"), filepath.Join(dir, "snapshot.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ClaimRole(role); err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *Tx) error {
		// Preserve coverage for rollback into a pre-compression database.
		if err := tx.Put("metadata", "format", databaseFormat{Role: role, SchemaVersion: 1}); err != nil {
			return err
		}
		if role == "agent" {
			if err := tx.Put("agent-identity", "self", map[string]string{"instanceId": "node", "credential": "qa-local", "enrollmentId": "original", "masterUrl": "https://qa.example"}); err != nil {
				return err
			}
		}
		if err := tx.Put("migration", "state", map[string]any{"pending": false, "migrationId": "qa", "expectedDatabase": path}); err != nil {
			return err
		}
		root, err := tx.tx.CreateBucket([]byte("allocator"))
		if err != nil {
			return err
		}
		if err = root.SetSequence(47); err != nil {
			return err
		}
		if err = root.Put([]byte{0, 1, 255}, []byte{1, 0, 255}); err != nil {
			return err
		}
		nested, err := root.CreateBucket([]byte("nested"))
		if err != nil {
			return err
		}
		if err = nested.SetSequence(1000); err != nil {
			return err
		}
		if err = nested.Put([]byte("empty"), []byte{}); err != nil {
			return err
		}
		deep, err := nested.CreateBucket([]byte("deep"))
		if err != nil {
			return err
		}
		if err = deep.SetSequence(9001); err != nil {
			return err
		}
		return deep.Put([]byte("value"), []byte("original"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = BackupOffline(path, role, backup); err != nil {
		t.Fatal(err)
	}
	return path, backup
}

func mutateRestoreFile(t *testing.T, path string, mutate func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Update(mutate); err != nil {
		t.Fatal(err)
	}
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestoreOfflineKeepsOriginalFileAndRestoresNestedSequences(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path, backup := restoreFixture(t, role)
			backupBefore := fileBytes(t, backup)
			mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
				if err := tx.DeleteBucket([]byte("allocator")); err != nil {
					return err
				}
				_, err := tx.CreateBucket([]byte("created-after-snapshot"))
				return err
			})
			before, _ := os.Stat(path)
			if err := RestoreOffline(path, role, backup); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(path)
			if !os.SameFile(before, after) {
				t.Fatal("restore replaced database inode")
			}
			if !bytes.Equal(backupBefore, fileBytes(t, backup)) {
				t.Fatal("restore modified backup")
			}
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = db.ClaimRole(role); err != nil {
				t.Fatal("restored fence rejects original destination", err)
			}
			err = db.View(func(tx *Tx) error {
				if tx.tx.Bucket([]byte("created-after-snapshot")) != nil {
					t.Fatal("new bucket survived rollback")
				}
				root := tx.tx.Bucket([]byte("allocator"))
				if root == nil || root.Sequence() != 47 || !bytes.Equal(root.Get([]byte{0, 1, 255}), []byte{1, 0, 255}) {
					t.Fatal("root contents or sequence changed")
				}
				nested := root.Bucket([]byte("nested"))
				if nested == nil || nested.Sequence() != 1000 || nested.Get([]byte("empty")) == nil {
					t.Fatal("nested sequence or empty value lost")
				}
				deep := nested.Bucket([]byte("deep"))
				if deep == nil || deep.Sequence() != 9001 || string(deep.Get([]byte("value"))) != "original" {
					t.Fatal("deep contents or sequence changed")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreOfflineRejectsActiveWriterWithoutChangingEitherFile(t *testing.T) {
	path, backup := restoreFixture(t, "master")
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	currentBefore, backupBefore := fileBytes(t, path), fileBytes(t, backup)
	if err = RestoreOffline(path, "master", backup); !errors.Is(err, bolt.ErrTimeout) {
		t.Fatal("active writer was not refused", err)
	}
	if !bytes.Equal(currentBefore, fileBytes(t, path)) || !bytes.Equal(backupBefore, fileBytes(t, backup)) {
		t.Fatal("rejected restore modified database or backup")
	}
	if err = live.Update(func(tx *Tx) error { return tx.Put("post-rejection", "alive", true) }); err != nil {
		t.Fatal("rejected restore disrupted original writer", err)
	}
}

func TestRestoreOfflineRejectsInvalidSourceAndDestination(t *testing.T) {
	for _, location := range []string{"database", "backup"} {
		for _, problem := range []string{"role", "schema", "pending", "missing-pending", "missing-migration-id", "wrong-final-path", "unenrolled"} {
			t.Run(location+"/"+problem, func(t *testing.T) {
				t.Parallel()
				role := "master"
				if problem == "unenrolled" {
					role = "agent"
				}
				path, backup := restoreFixture(t, role)
				changed := path
				if location == "backup" {
					changed = backup
				}
				mutateRestoreFile(t, changed, func(raw *bolt.Tx) error {
					tx := &Tx{tx: raw}
					switch problem {
					case "role":
						return tx.Put("metadata", "format", map[string]any{"role": "other", "schemaVersion": 1})
					case "schema":
						return tx.Put("metadata", "format", map[string]any{"role": role, "schemaVersion": SchemaTaskLeases + 1})
					case "pending":
						return tx.Put("migration", "state", map[string]any{"pending": true, "migrationId": "qa"})
					case "missing-pending":
						return tx.Put("migration", "state", map[string]any{"migrationId": "qa"})
					case "missing-migration-id":
						return tx.Put("migration", "state", map[string]any{"pending": false})
					case "wrong-final-path":
						return tx.Put("migration", "state", map[string]any{"pending": false, "migrationId": "qa", "expectedDatabase": path + "-other"})
					default:
						return tx.Delete("agent-identity", "self")
					}
				})
				currentBefore, backupBefore := fileBytes(t, path), fileBytes(t, backup)
				if err := RestoreOffline(path, role, backup); err == nil {
					t.Fatal("invalid restore accepted")
				}
				if !bytes.Equal(currentBefore, fileBytes(t, path)) || !bytes.Equal(backupBefore, fileBytes(t, backup)) {
					t.Fatal("invalid restore changed either file")
				}
			})
		}
	}
}

func TestRestoreOfflineRejectsMissingEmptyAndSameFiles(t *testing.T) {
	for _, problem := range []string{"missing-database", "missing-backup", "empty-database", "empty-backup", "same-path", "hardlink", "symlink"} {
		t.Run(problem, func(t *testing.T) {
			path, backup := restoreFixture(t, "master")
			switch problem {
			case "missing-database":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "missing-backup":
				if err := os.Remove(backup); err != nil {
					t.Fatal(err)
				}
			case "empty-database":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "empty-backup":
				if err := os.WriteFile(backup, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "same-path":
				backup = path
			case "hardlink":
				backup += "-link"
				if err := os.Link(path, backup); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				backup += "-link"
				if err := os.Symlink(path, backup); err != nil {
					t.Skip("symlinks not available to this test account")
				}
			}
			currentBefore, currentErr := os.ReadFile(path)
			backupBefore, backupErr := os.ReadFile(backup)
			if err := RestoreOffline(path, "master", backup); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			currentAfter, currentAfterErr := os.ReadFile(path)
			backupAfter, backupAfterErr := os.ReadFile(backup)
			if !bytes.Equal(currentBefore, currentAfter) || !bytes.Equal(backupBefore, backupAfter) || (currentErr != nil) != (currentAfterErr != nil) || (backupErr != nil) != (backupAfterErr != nil) {
				t.Fatal("failed restore changed or initialized a file")
			}
		})
	}
}

func TestRestoreTransactionFailureRollsBackDeletionAndCopiedBuckets(t *testing.T) {
	path, backup := restoreFixture(t, "master")
	mutateRestoreFile(t, path, func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte("keep-current")); return err })
	before, backupBefore := fileBytes(t, path), fileBytes(t, backup)
	destination, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	source, err := bolt.Open(backup, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	injected := errors.New("injected error before commit")
	err = source.View(func(raw *bolt.Tx) error {
		return destination.Update(func(tx *bolt.Tx) error {
			if err := restoreTransaction(tx, raw); err != nil {
				return err
			}
			return injected
		})
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, fileBytes(t, path)) || !bytes.Equal(backupBefore, fileBytes(t, backup)) {
		t.Fatal("failed transaction changed either file")
	}
	if err = destination.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("keep-current")) == nil {
			t.Fatal("failed restore committed bucket deletion")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
