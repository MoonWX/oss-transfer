package state

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

const credentialBucket = "storage-configuration"
const encryptionMetadataKey = "credential-encryption"

var encryptedMagic = []byte{0xfe, 'O', 'S', 'T', 'S', 'E', 'C', 1}
var ErrCredentialEncryption = errors.New("storage credential encryption is invalid; restore the matching database and key file")

type credentialCipher struct {
	key  []byte
	id   string
	aead cipher.AEAD
}

type encryptionMetadata struct {
	Version int    `json:"version"`
	KeyID   string `json:"keyId"`
}

// CredentialKeyPath is paired with a database or an offline snapshot. Both files
// belong in the same private data directory and must be kept together on backup.
func CredentialKeyPath(database string) string { return database + ".key" }

func newCredentialCipher(key []byte) (*credentialCipher, error) {
	if len(key) != 32 {
		return nil, ErrCredentialEncryption
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(key)
	return &credentialCipher{key: bytes.Clone(key), id: hex.EncodeToString(digest[:]), aead: aead}, nil
}

func readCredentialKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, ErrCredentialEncryption
	}
	if err := checkPrivateCredentialKey(path, info); err != nil {
		return nil, fmt.Errorf("credential key permissions: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !os.SameFile(current, opened) || !current.Mode().IsRegular() {
		return nil, ErrCredentialEncryption
	}
	key, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil || len(key) != 32 {
		return nil, ErrCredentialEncryption
	}
	return key, nil
}

// Publish only a fully written and synced key. A partial creation can never be
// mistaken for a new key after an interrupted first startup.
func writeCredentialKey(path string, key []byte) error {
	if len(key) != 32 {
		return ErrCredentialEncryption
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".credential-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := protectCredentialKey(file); err != nil {
		return err
	}
	if _, err := file.Write(key); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return publishCompact(file.Name(), path)
}

func loadCredentialCipher(path string, required bool) (*credentialCipher, error) {
	key, err := readCredentialKey(CredentialKeyPath(path))
	if errors.Is(err, os.ErrNotExist) && !required {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := writeCredentialKey(CredentialKeyPath(path), key); err != nil {
			return nil, fmt.Errorf("create credential key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read credential key %s: %w", CredentialKeyPath(path), err)
	}
	return newCredentialCipher(key)
}

func encryptionInfo(raw *bolt.Tx) (encryptionMetadata, bool, error) {
	var info encryptionMetadata
	b := raw.Bucket([]byte("metadata"))
	if b == nil || b.Get([]byte(encryptionMetadataKey)) == nil {
		return info, false, nil
	}
	value, err := decodeValue(b.Get([]byte(encryptionMetadataKey)))
	if err != nil || json.Unmarshal(value, &info) != nil || info.Version != 1 || len(info.KeyID) != 64 {
		return info, true, ErrCredentialEncryption
	}
	return info, true, nil
}

func credentialAAD(bucket, key string) []byte {
	return []byte("oss-transfer/storage-credentials/v1\x00" + bucket + "\x00" + key)
}

func (c *credentialCipher) seal(bucket, key string, plain []byte) ([]byte, error) {
	if c == nil {
		return nil, ErrCredentialEncryption
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := append(bytes.Clone(encryptedMagic), nonce...)
	return c.aead.Seal(header, nonce, plain, credentialAAD(bucket, key)), nil
}

func (c *credentialCipher) open(bucket, key string, stored []byte) ([]byte, error) {
	if c == nil || !bytes.HasPrefix(stored, encryptedMagic) || len(stored) < len(encryptedMagic)+c.aead.NonceSize()+c.aead.Overhead() {
		return nil, ErrCredentialEncryption
	}
	offset := len(encryptedMagic)
	plain, err := c.aead.Open(nil, stored[offset:offset+c.aead.NonceSize()], stored[offset+c.aead.NonceSize():], credentialAAD(bucket, key))
	if err != nil {
		return nil, ErrCredentialEncryption
	}
	return plain, nil
}

func (t *Tx) decodeStored(bucket, key string, stored []byte) ([]byte, error) {
	if bucket == credentialBucket {
		info, encrypted, err := encryptionInfo(t.tx)
		if err != nil {
			return nil, err
		}
		if encrypted {
			if t.credentials == nil || info.KeyID != t.credentials.id {
				return nil, ErrCredentialEncryption
			}
			stored, err = t.credentials.open(bucket, key, stored)
			if err != nil {
				return nil, err
			}
		} else if bytes.HasPrefix(stored, encryptedMagic) {
			return nil, ErrCredentialEncryption
		}
	}
	return decodeValue(stored)
}

func (t *Tx) ensureCredentialEncryption(recordKey string) error {
	if t.credentials == nil {
		return ErrCredentialEncryption
	}
	info, found, err := encryptionInfo(t.tx)
	if err != nil {
		return err
	}
	if found && info.KeyID != t.credentials.id {
		return ErrCredentialEncryption
	}
	var format databaseFormat
	hasFormat, err := t.Get("metadata", "format", &format)
	if err != nil {
		return err
	}
	if !hasFormat {
		if recordKey != "master" && recordKey != "agent" {
			return errors.New("storage configuration has no database role")
		}
		if err := t.Put("metadata", "format", databaseFormat{Role: recordKey, SchemaVersion: SchemaEncryptedCredentials}); err != nil {
			return err
		}
	} else if err := RequireSchema(t, format.Role, SchemaEncryptedCredentials); err != nil {
		return err
	}
	if !found {
		return t.Put("metadata", encryptionMetadataKey, encryptionMetadata{Version: 1, KeyID: t.credentials.id})
	}
	return nil
}

// Every persistent storage-credential record passes through this bucket, including
// the legacy importer. Task and inventory records contain aliases or fingerprints;
// transport command payloads stay in memory and journals save only results.
func (s *Store) initializeCredentials(path string) error {
	var metadata encryptionMetadata
	var encrypted bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		if bucket := tx.Bucket([]byte("metadata")); bucket != nil && bucket.Get([]byte("format")) != nil {
			var format databaseFormat
			if json.Unmarshal(bucket.Get([]byte("format")), &format) != nil || (format.Role != "master" && format.Role != "agent") || !supportedSchema(format.SchemaVersion) {
				return errors.New("database has an invalid role or unsupported schema")
			}
		}
		var err error
		metadata, encrypted, err = encryptionInfo(tx)
		return err
	}); err != nil {
		return err
	}
	credentials, err := loadCredentialCipher(path, encrypted)
	if err != nil {
		return err
	}
	if encrypted && credentials.id != metadata.KeyID {
		return ErrCredentialEncryption
	}
	if encrypted {
		if err := s.db.View(func(raw *bolt.Tx) error {
			var format databaseFormat
			b := raw.Bucket([]byte("metadata"))
			if b == nil || json.Unmarshal(b.Get([]byte("format")), &format) != nil || format.SchemaVersion < SchemaEncryptedCredentials || !supportedSchema(format.SchemaVersion) {
				return ErrCredentialEncryption
			}
			return nil
		}); err != nil {
			return err
		}
	}
	s.credentials = credentials
	return s.Update(func(tx *Tx) error {
		bucket := tx.tx.Bucket([]byte(credentialBucket))
		if bucket == nil {
			return nil
		}
		type record struct {
			key   string
			value []byte
		}
		var records []record
		if err := bucket.ForEach(func(key, stored []byte) error {
			if stored == nil {
				return ErrCredentialEncryption
			}
			value, err := tx.decodeStored(credentialBucket, string(key), stored)
			if err != nil || !json.Valid(value) {
				return ErrCredentialEncryption
			}
			if !encrypted {
				records = append(records, record{string(key), bytes.Clone(value)})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, record := range records {
			encoded, err := encodeValue(record.value)
			if err != nil {
				return err
			}
			sealed, err := credentials.seal(credentialBucket, record.key, encoded)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(record.key), sealed); err != nil {
				return err
			}
			tx.dirty = true
		}
		if len(records) > 0 {
			return tx.ensureCredentialEncryption(records[0].key)
		}
		return nil
	})
}

// Offline tools never invent a replacement key for an encrypted database.
func offlineCredentialCipher(raw *bolt.Tx, path string) (*credentialCipher, error) {
	info, encrypted, err := encryptionInfo(raw)
	if err != nil || !encrypted {
		return nil, err
	}
	credentials, err := loadCredentialCipher(path, true)
	if err != nil {
		return nil, err
	}
	if credentials.id != info.KeyID {
		return nil, ErrCredentialEncryption
	}
	tx := &Tx{tx: raw, credentials: credentials}
	if b := raw.Bucket([]byte(credentialBucket)); b != nil {
		err = b.ForEach(func(key, value []byte) error {
			decoded, err := tx.decodeStored(credentialBucket, string(key), value)
			if err != nil || !json.Valid(decoded) {
				return ErrCredentialEncryption
			}
			return nil
		})
	}
	return credentials, err
}

func (t *Tx) copyDecodedStored(bucket, key string, stored []byte) ([]byte, error) {
	if bucket != credentialBucket {
		return copyDecodedValue(stored)
	}
	decoded, err := t.decodeStored(bucket, key, stored)
	return bytes.Clone(decoded), err
}
