// Package state provides durable transactions shared by the Master and Agent.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrLegacyDatabase = errors.New("legacy RocksDB database detected; export and import it before starting the Go service")
var ErrMigrationPending = errors.New("offline migration is incomplete; finish migration before starting this database")

type Store struct{ db *bolt.DB }
type Tx struct{ tx *bolt.Tx }

// SchemaResolvedSources prevents older readers from discarding identities pinned
// after a missing inventory object becomes available.
const SchemaResolvedSources = 2

type databaseFormat struct {
	Role          string `json:"role"`
	SchemaVersion int    `json:"schemaVersion"`
}

func supportedSchema(version int) bool { return version >= 1 && version <= SchemaResolvedSources }

// RequireSchema raises the minimum reader version in the same transaction as
// the data requiring it. A failed transaction preserves both the old format and
// the old data. Ordinary databases stay at version 1 until a feature needs more.
func RequireSchema(tx *Tx, role string, minimum int) error {
	if role != "master" && role != "agent" || !supportedSchema(minimum) {
		return errors.New("invalid database format requirement")
	}
	var metadata databaseFormat
	found, err := tx.Get("metadata", "format", &metadata)
	if err != nil {
		return err
	}
	if !found || metadata.Role != role || !supportedSchema(metadata.SchemaVersion) {
		return errors.New("database has no matching role or supported schema")
	}
	if metadata.SchemaVersion >= minimum {
		return nil
	}
	metadata.SchemaVersion = minimum
	return tx.Put("metadata", "format", metadata)
}

// ClaimRole prevents either process from opening the other process's database.
func (s *Store) ClaimRole(role string) error {
	if role != "master" && role != "agent" {
		return errors.New("invalid database role")
	}
	return s.Update(func(tx *Tx) error {
		var migration struct {
			Pending          *bool  `json:"pending"`
			MigrationID      string `json:"migrationId"`
			ExpectedDatabase string `json:"expectedDatabase,omitempty"`
		}
		migrationFound, err := tx.Get("migration", "state", &migration)
		if err != nil {
			return err
		}
		if migrationFound && (migration.Pending == nil || *migration.Pending || migration.MigrationID == "" || migration.ExpectedDatabase != "" && filepath.Clean(migration.ExpectedDatabase) != filepath.Clean(s.db.Path())) {
			return ErrMigrationPending
		}
		var metadata databaseFormat
		found, err := tx.Get("metadata", "format", &metadata)
		if err != nil {
			return err
		}
		if found {
			if metadata.Role != role || !supportedSchema(metadata.SchemaVersion) {
				return errors.New("database belongs to another role or unsupported schema")
			}
			return nil
		}
		metadata.Role = role
		metadata.SchemaVersion = 1
		return tx.Put("metadata", "format", metadata)
	})
}

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	// Do not silently create a new database next to an existing V4 database.
	for _, candidate := range []string{dir, filepath.Join(dir, "rocksdb"), filepath.Join(filepath.Dir(dir), "rocksdb")} {
		if _, err := os.Stat(filepath.Join(candidate, "CURRENT")); err == nil {
			return nil, ErrLegacyDatabase
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	// Default bbolt commits fsync; disabling it would invalidate stop/ACK fences.
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) View(fn func(*Tx) error) error {
	return s.db.View(func(tx *bolt.Tx) error { return fn(&Tx{tx: tx}) })
}
func (s *Store) Update(fn func(*Tx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error { return fn(&Tx{tx: tx}) })
}

func (t *Tx) Get(bucket, key string, into any) (bool, error) {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return false, nil
	}
	value := b.Get([]byte(key))
	if value == nil {
		return false, nil
	}
	if err := json.Unmarshal(value, into); err != nil {
		return false, fmt.Errorf("decode %s/%s: %w", bucket, key, err)
	}
	return true, nil
}

func (t *Tx) Put(bucket, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b, err := t.tx.CreateBucketIfNotExists([]byte(bucket))
	if err != nil {
		return err
	}
	return b.Put([]byte(key), encoded)
}

func (t *Tx) Delete(bucket, key string) error {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return nil
	}
	return b.Delete([]byte(key))
}

func (t *Tx) NextID(bucket string) (uint64, error) {
	b, err := t.tx.CreateBucketIfNotExists([]byte(bucket))
	if err != nil {
		return 0, err
	}
	return b.NextSequence()
}

// ReserveID advances an allocator without allocating every intervening ID.
// Importers call it in the same transaction that saves imported rows and indexes.
// A smaller reservation never moves the allocator backwards.
func (t *Tx) ReserveID(bucket string, minimum uint64) error {
	b, err := t.tx.CreateBucketIfNotExists([]byte(bucket))
	if err != nil {
		return err
	}
	if b.Sequence() >= minimum {
		return nil
	}
	return b.SetSequence(minimum)
}

// Scan visits keys in order. after is exclusive and limit bounds memory use.
// Values are copied so callers cannot retain pointers into a closed transaction.
func (t *Tx) Scan(bucket, prefix, after string, limit int, fn func(string, json.RawMessage) error) error {
	if limit <= 0 {
		return errors.New("scan limit must be positive")
	}
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return nil
	}
	c := b.Cursor()
	start := prefix
	if after > start {
		start = after
	}
	k, v := c.Seek([]byte(start))
	if after != "" && string(k) == after {
		k, v = c.Next()
	}
	for count := 0; k != nil && bytes.HasPrefix(k, []byte(prefix)) && count < limit; count++ {
		if v != nil {
			if err := fn(string(k), append(json.RawMessage(nil), v...)); err != nil {
				return err
			}
		}
		k, v = c.Next()
	}
	return nil
}
