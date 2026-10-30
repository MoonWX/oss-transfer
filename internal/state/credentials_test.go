package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

var credentialFixture = map[string]any{
	"revision": float64(17),
	"connections": []any{map[string]any{
		"alias": "test", "endpoint": "https://storage.invalid",
		"accessKey": "fixture-secret-access-key-6a29e", "secretKey": "fixture-secret-signing-key-3b8fd",
		"sessionToken": "fixture-secret-session-token-7ed94",
	}},
}

func putCredentialFixture(t *testing.T, db *Store, role string) {
	t.Helper()
	if err := db.Update(func(tx *Tx) error { return tx.Put(credentialBucket, role, credentialFixture) }); err != nil {
		t.Fatal(err)
	}
}
func checkCredentialFixture(t *testing.T, db *Store, role string) {
	t.Helper()
	var value map[string]any
	if err := db.View(func(tx *Tx) error {
		ok, err := tx.Get(credentialBucket, role, &value)
		if err == nil && !ok {
			return errors.New("credential record missing")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, credentialFixture) {
		t.Fatal("credential round trip changed fields")
	}
}
func assertNoCredentialPlaintext(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	connection := credentialFixture["connections"].([]any)[0].(map[string]any)
	for _, field := range []string{"accessKey", "secretKey", "sessionToken"} {
		if bytes.Contains(raw, []byte(connection[field].(string))) {
			t.Fatalf("%s persisted in plaintext", field)
		}
	}
}
func TestCredentialsEncryptedAtRestAndStableAcrossRestart(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), role+".db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ClaimRole(role); err != nil {
				t.Fatal(err)
			}
			putCredentialFixture(t, db, role)
			checkCredentialFixture(t, db, role)
			id := transactionID(t, db)
			for range 5 {
				putCredentialFixture(t, db, role)
			}
			if transactionID(t, db) != id {
				t.Fatal("unchanged credentials caused extra writes")
			}
			if readFormat(t, db).SchemaVersion < SchemaEncryptedCredentials {
				t.Fatal("old binaries are not fenced")
			}
			if err := db.View(func(tx *Tx) error {
				for _, reverse := range []bool{false, true} {
					count := 0
					fn := func(_ string, raw json.RawMessage) error {
						var value map[string]any
						if err := json.Unmarshal(raw, &value); err != nil {
							return err
						}
						if !reflect.DeepEqual(value, credentialFixture) {
							t.Fatal("scan decrypted incorrectly")
						}
						count++
						return nil
					}
					var err error
					if reverse {
						err = tx.ScanReverse(credentialBucket, "", 10, fn)
					} else {
						err = tx.Scan(credentialBucket, "", "", 10, fn)
					}
					if err != nil {
						return err
					}
					if count != 1 {
						t.Fatal("missing scan result")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			db.Close()
			assertNoCredentialPlaintext(t, path)
			key, err := os.ReadFile(CredentialKeyPath(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(key) != 32 {
				t.Fatal("invalid AES-256 key length")
			}
			info, _ := os.Stat(CredentialKeyPath(path))
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("credential key is not private")
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			checkCredentialFixture(t, db, role)
			reopenedKey, _ := os.ReadFile(CredentialKeyPath(path))
			if !bytes.Equal(key, reopenedKey) {
				t.Fatal("restart rotated the key")
			}
		})
	}
}

func TestLegacyCredentialsMigrateAndCompactionRemovesHistoricPlaintext(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path, _ := restoreFixture(t, role)
			raw, _ := json.Marshal(credentialFixture)
			mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists([]byte(credentialBucket))
				if err != nil {
					return err
				}
				return b.Put([]byte(role), raw)
			})
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			checkCredentialFixture(t, db, role)
			if readFormat(t, db).SchemaVersion != SchemaEncryptedCredentials {
				t.Fatal("migration did not fence legacy readers")
			}
			if err := db.db.View(func(tx *bolt.Tx) error {
				if !bytes.HasPrefix(tx.Bucket([]byte(credentialBucket)).Get([]byte(role)), encryptedMagic) {
					t.Fatal("legacy record not encrypted")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			db.Close()
			compact := filepath.Join(t.TempDir(), "compacted.db")
			if err := CompactOffline(path, role, compact); err != nil {
				t.Fatal(err)
			}
			assertNoCredentialPlaintext(t, compact)
			moved, err := Open(compact)
			if err != nil {
				t.Fatal(err)
			}
			defer moved.Close()
			checkCredentialFixture(t, moved, role)
		})
	}
}

func TestCredentialCorruptionFailsClosedWithoutChangingDatabase(t *testing.T) {
	for _, scenario := range []string{"missing-key", "wrong-key", "short-key", "symlink-key", "wide-permissions", "ciphertext", "nonce", "record-name"} {
		t.Run(scenario, func(t *testing.T) {
			if runtime.GOOS == "windows" && (scenario == "symlink-key" || scenario == "wide-permissions") {
				t.Skip("Unix file protection")
			}
			path := filepath.Join(t.TempDir(), "master.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			putCredentialFixture(t, db, "master")
			db.Close()
			keyPath := CredentialKeyPath(path)
			switch scenario {
			case "missing-key":
				err = os.Remove(keyPath)
			case "wrong-key":
				err = os.WriteFile(keyPath, bytes.Repeat([]byte{1}, 32), 0600)
			case "short-key":
				err = os.WriteFile(keyPath, []byte("invalid"), 0600)
			case "wide-permissions":
				err = os.Chmod(keyPath, 0644)
			case "symlink-key":
				other := keyPath + ".original"
				if err = os.Rename(keyPath, other); err == nil {
					err = os.Symlink(other, keyPath)
				}
			default:
				mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(credentialBucket))
					value := bytes.Clone(b.Get([]byte("master")))
					if scenario == "record-name" {
						if err := b.Delete([]byte("master")); err != nil {
							return err
						}
						return b.Put([]byte("agent"), value)
					}
					index := len(value) - 1
					if scenario == "nonce" {
						index = len(encryptedMagic)
					}
					value[index] ^= 1
					return b.Put([]byte("master"), value)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := fileBytes(t, path)
			opened, err := Open(path)
			if err == nil {
				opened.Close()
				t.Fatal("damaged credentials were accepted")
			}
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("failed startup changed database")
			}
			if scenario == "missing-key" {
				if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing key was silently replaced")
				}
			}
		})
	}
}

func TestEncryptedBackupRestoreAndCompactKeepMatchingKeys(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path, _ := restoreFixture(t, role)
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *Tx) error { return tx.Delete("migration", "state") }); err != nil {
				t.Fatal(err)
			}
			putCredentialFixture(t, db, role)
			db.Close()
			snapshot := filepath.Join(t.TempDir(), "database")
			if err := BackupOffline(path, role, snapshot); err != nil {
				t.Fatal(err)
			}
			assertNoCredentialPlaintext(t, snapshot)
			// A backup is usable after the source directory and its key disappear.
			if err := os.Remove(CredentialKeyPath(path)); err != nil {
				t.Fatal(err)
			}
			copy, err := Open(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			checkCredentialFixture(t, copy, role)
			copy.Close()
			destination, oldBackup := restoreFixture(t, role)
			other, err := Open(destination)
			if err != nil {
				t.Fatal(err)
			}
			putCredentialFixture(t, other, role)
			other.Close()
			keyBefore, _ := os.ReadFile(CredentialKeyPath(destination))
			if err := RestoreOffline(destination, role, snapshot); err != nil {
				t.Fatal(err)
			}
			restored, err := Open(destination)
			if err != nil {
				t.Fatal(err)
			}
			checkCredentialFixture(t, restored, role)
			restored.Close()
			keyAfter, _ := os.ReadFile(CredentialKeyPath(destination))
			if !bytes.Equal(keyBefore, keyAfter) {
				t.Fatal("restore replaced the live key non-atomically")
			}
			// Rollback to a pre-encryption snapshot retains that snapshot's old schema.
			if err := RestoreOffline(destination, role, oldBackup); err != nil {
				t.Fatal(err)
			}
			rolledBack, err := Open(destination)
			if err != nil {
				t.Fatal(err)
			}
			defer rolledBack.Close()
			if readFormat(t, rolledBack).SchemaVersion != 1 {
				t.Fatal("rollback changed the old snapshot schema")
			}
		})
	}
}

func TestEncryptedCompressedCredentialSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	value := map[string]string{"accessKey": strings.Repeat("private-value-", 300), "secretKey": strings.Repeat("second-private-value-", 300)}
	if err := db.Update(func(tx *Tx) error { return tx.Put(credentialBucket, "master", value) }); err != nil {
		t.Fatal(err)
	}
	var read map[string]string
	if err := db.View(func(tx *Tx) error { _, err := tx.Get(credentialBucket, "master", &read); return err }); err != nil || !reflect.DeepEqual(read, value) {
		t.Fatal("compressed encrypted credentials did not round trip", err)
	}
}

func TestCredentialOfflineToolsRejectMissingOrWrongKeys(t *testing.T) {
	for _, operation := range []string{"backup", "compact", "restore"} {
		t.Run(operation, func(t *testing.T) {
			path, _ := restoreFixture(t, "master")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			putCredentialFixture(t, db, "master")
			db.Close()
			snapshot := filepath.Join(t.TempDir(), "database")
			if err := BackupOffline(path, "master", snapshot); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(CredentialKeyPath(path)); err != nil {
				t.Fatal(err)
			}
			before := fileBytes(t, path)
			output := filepath.Join(t.TempDir(), "output")
			switch operation {
			case "backup":
				err = BackupOffline(path, "master", output)
			case "compact":
				err = CompactOffline(path, "master", output)
			case "restore":
				err = RestoreOffline(path, "master", snapshot)
			}
			if err == nil {
				t.Fatal("missing live key was ignored")
			}
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("rejected operation changed the database")
			}
			if _, err := os.Lstat(CredentialKeyPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("operation invented a replacement live key")
			}
			// A matching snapshot key explicitly restores recovery, without changing
			// the data or making an irreversible choice between unrelated keys.
			key, err := readCredentialKey(CredentialKeyPath(snapshot))
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCredentialKey(CredentialKeyPath(path), key); err != nil {
				t.Fatal(err)
			}
			if err := RestoreOffline(path, "master", snapshot); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			checkCredentialFixture(t, reopened, "master")
		})
	}
}

func TestCredentialEncryptionDoesNotTouchFutureDatabase(t *testing.T) {
	path, _ := restoreFixture(t, "master")
	// Simulate a newer deployment for which no key has yet been provisioned.
	_ = os.Remove(CredentialKeyPath(path))
	mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("metadata")).Put([]byte("format"), []byte(`{"role":"master","schemaVersion":999}`))
	})
	before := fileBytes(t, path)
	db, err := Open(path)
	if err == nil {
		db.Close()
		t.Fatal("future schema opened")
	}
	if !bytes.Equal(before, fileBytes(t, path)) {
		t.Fatal("future database was changed")
	}
	if _, err := os.Lstat(CredentialKeyPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("future database was given a new key")
	}
}

func TestEncryptedRestoreRejectsTamperedSnapshotKeyBeforeMutation(t *testing.T) {
	path, _ := restoreFixture(t, "master")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	putCredentialFixture(t, db, "master")
	db.Close()
	snapshot := filepath.Join(t.TempDir(), "database")
	if err := BackupOffline(path, "master", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CredentialKeyPath(snapshot), bytes.Repeat([]byte{9}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, path)
	if err := RestoreOffline(path, "master", snapshot); !errors.Is(err, ErrCredentialEncryption) {
		t.Fatal("wrong snapshot key was not rejected", err)
	}
	if !bytes.Equal(before, fileBytes(t, path)) {
		t.Fatal("invalid snapshot changed the live database")
	}
}
