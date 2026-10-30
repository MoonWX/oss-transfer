package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

const compactTransactionBytes = 8 << 20
const compactSpaceReserve = 256 << 20

var ErrCompactSpace = errors.New("insufficient disk space for database compaction")

func checkCompactSpace(available uint64, pendingBytes int64) error {
	// Keep room for the running installation and bbolt file growth. This is
	// rechecked before every commit; compression estimates are never trusted.
	if pendingBytes < 0 {
		return ErrCompactSpace
	}
	needed := uint64(compactSpaceReserve) + 32<<20 + 2*uint64(pendingBytes)
	if available < needed {
		return fmt.Errorf("%w: need %s available, found %s", ErrCompactSpace, backupSize(int64(needed)), backupSize(int64(available)))
	}
	return nil
}

// CompactOffline writes a validated, compressed copy to a new path. It never
// replaces the source: services must be stopped before an operator installs it.
func CompactOffline(path, role, destination string) error {
	return CompactOfflineWithProgress(path, role, destination, nil)
}

// CompactOptions can normalize a known legacy representation while copying.
// Normalize must be deterministic and must not mutate its inputs. Returning nil
// is an error: this hook cannot remove records or change bucket identities.
type CompactOptions struct {
	credentials *credentialCipher
	Progress    io.Writer
	Normalize   func(bucketPath []string, key string, decoded []byte) ([]byte, error)
}

func CompactOfflineWithProgress(path, role, destination string, progress io.Writer) error {
	return CompactOfflineWithOptions(path, role, destination, CompactOptions{Progress: progress})
}

func CompactOfflineWithOptions(path, role, destination string, options CompactOptions) error {
	progress := options.Progress
	if role != "master" && role != "agent" {
		return errors.New("invalid database role")
	}
	info, err := offlineFileInfo(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("compact destination already exists")
	}
	source, err := openExistingOffline(path, info, true)
	if err != nil {
		return fmt.Errorf("open offline database (stop its service first): %w", err)
	}
	defer source.Close()
	return source.View(func(original *bolt.Tx) error {
		if err := validateOfflineTransaction(original, role, path); err != nil {
			return err
		}
		credentials, err := offlineCredentialCipher(original, path)
		if err != nil {
			return err
		}
		options.credentials = credentials
		if _, err := os.Lstat(CredentialKeyPath(destination)); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return errors.New("compact destination key already exists")
		}
		available, err := compactFreeBytes(filepath.Dir(destination))
		if err != nil {
			return err
		}
		if err := checkCompactSpace(available, 0); err != nil {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".compact-*")
		if err != nil {
			return err
		}
		temporary := file.Name()
		defer os.Remove(temporary)
		if err := file.Close(); err != nil {
			return err
		}
		output, err := bolt.Open(temporary, 0600, nil)
		if err != nil {
			return err
		}
		defer output.Close()
		writer := compactWriter{db: output, credentials: credentials, progress: progress, started: time.Now(), reported: time.Now(), normalize: options.Normalize,
			available: func() (uint64, error) { return compactFreeBytes(filepath.Dir(destination)) },
		}
		defer func() {
			if writer.tx != nil {
				_ = writer.tx.Rollback()
			}
		}()
		if progress != nil {
			fmt.Fprintf(progress, "Compacting database: %s.\n", backupSize(info.Size()))
		}
		if err := original.ForEach(func(name []byte, b *bolt.Bucket) error { return writer.bucket([][]byte{name}, b) }); err != nil {
			return err
		}
		if err := writer.ensureTransaction(); err != nil {
			return err
		}
		metadata := writer.tx.Bucket([]byte("metadata"))
		format, err := compactFormat(metadata.Get([]byte("format")))
		if err != nil {
			return err
		}
		if err := metadata.Put([]byte("format"), format); err != nil {
			return err
		}
		if err := writer.commit(); err != nil {
			return err
		}
		if progress != nil {
			fmt.Fprintln(progress, "Verifying compacted database.")
		}
		if err := output.View(func(copy *bolt.Tx) error {
			if err := validateOfflineTransaction(copy, role, path); err != nil {
				return err
			}
			return compareCompactTransactionsWithOptions(original, copy, options)
		}); err != nil {
			return fmt.Errorf("verify compacted database: %w", err)
		}
		if err := output.Sync(); err != nil {
			return err
		}
		if err := output.Close(); err != nil {
			return err
		}
		result, err := os.Stat(temporary)
		if err != nil {
			return err
		}
		if credentials != nil {
			if err := writeCredentialKey(CredentialKeyPath(destination), credentials.key); err != nil {
				return err
			}
		}
		if err := publishCompact(temporary, destination); err != nil {
			if credentials != nil {
				_ = os.Remove(CredentialKeyPath(destination))
			}
			return err
		}
		if progress != nil {
			fmt.Fprintf(progress, "Database compaction complete: %s -> %s (%s).\n", backupSize(info.Size()), backupSize(result.Size()), time.Since(writer.started).Round(time.Second))
		}
		return nil
	})
}

type compactWriter struct {
	credentials       *credentialCipher
	normalize         func([]string, string, []byte) ([]byte, error)
	available         func() (uint64, error)
	db                *bolt.DB
	tx                *bolt.Tx
	bytes             int64
	processed         int64
	records           int64
	progress          io.Writer
	started, reported time.Time
}

func (w *compactWriter) ensureTransaction() error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.db.Begin(true)
	if err != nil {
		return err
	}
	w.tx = tx
	w.bytes = 0
	return nil
}
func (w *compactWriter) commit() error {
	if w.tx == nil {
		return nil
	}
	available, err := w.available()
	if err != nil {
		return err
	}
	if err := checkCompactSpace(available, w.bytes); err != nil {
		return err
	}
	tx := w.tx
	w.tx = nil
	return tx.Commit()
}
func (w *compactWriter) destination(path [][]byte) (*bolt.Bucket, error) {
	if err := w.ensureTransaction(); err != nil {
		return nil, err
	}
	b, err := w.tx.CreateBucketIfNotExists(path[0])
	if err != nil {
		return nil, err
	}
	for _, name := range path[1:] {
		b, err = b.CreateBucketIfNotExists(name)
		if err != nil {
			return nil, err
		}
	}
	b.FillPercent = 1
	return b, nil
}
func (w *compactWriter) bucket(path [][]byte, source *bolt.Bucket) error {
	if w.bytes >= compactTransactionBytes {
		if err := w.commit(); err != nil {
			return err
		}
	}
	target, err := w.destination(path)
	if err != nil {
		return err
	}
	if err := target.SetSequence(source.Sequence()); err != nil {
		return err
	}
	w.bytes += int64(len(path[len(path)-1])) + 32
	names := make([]string, len(path))
	for i, part := range path {
		names[i] = string(part)
	}
	return source.ForEach(func(key, stored []byte) error {
		if stored == nil {
			return w.bucket(append(path[:len(path):len(path)], key), source.Bucket(key))
		}
		decoded, err := decodeCompactCredential(names, string(key), stored, w.credentials)
		if err != nil {
			return err
		}
		if w.normalize != nil {
			decoded, err = w.normalize(names, string(key), decoded)
			if err != nil {
				return err
			}
			if decoded == nil {
				return errors.New("compact normalization cannot remove a record")
			}
		}
		encoded, err := encodeValue(decoded)
		if err != nil {
			return err
		}
		if len(path) == 1 && string(path[0]) == credentialBucket && w.credentials != nil {
			encoded, err = w.credentials.seal(credentialBucket, string(key), encoded)
			if err != nil {
				return err
			}
		}
		if len(path) == 1 && bytes.Equal(path[0], []byte("metadata")) && bytes.Equal(key, []byte("format")) {
			encoded = decoded // The format fence must remain readable by old binaries.
		}
		if w.bytes > 0 && w.bytes+int64(len(key)+len(encoded)) > compactTransactionBytes {
			if err := w.commit(); err != nil {
				return err
			}
		}
		target, err := w.destination(path)
		if err != nil {
			return err
		}
		if err := target.Put(key, encoded); err != nil {
			return err
		}
		w.bytes += int64(len(key) + len(encoded))
		w.processed += int64(len(stored))
		w.records++
		if w.progress != nil && time.Since(w.reported) >= 10*time.Second {
			fmt.Fprintf(w.progress, "Compacting database: %d records, %s read (%s elapsed).\n", w.records, backupSize(w.processed), time.Since(w.started).Round(time.Second))
			w.reported = time.Now()
		}
		return nil
	})
}
func compareCompactTransactions(source, destination *bolt.Tx) error {
	return compareCompactTransactionsWithOptions(source, destination, CompactOptions{})
}

func compareCompactTransactionsWithOptions(source, destination *bolt.Tx, options CompactOptions) error {
	a, b := source.Cursor(), destination.Cursor()
	ak, _ := a.First()
	bk, _ := b.First()
	for ak != nil || bk != nil {
		if !bytes.Equal(ak, bk) {
			return errors.New("top-level buckets changed")
		}
		if err := compareCompactBuckets(source.Bucket(ak), destination.Bucket(bk), []string{string(ak)}, options); err != nil {
			return err
		}
		ak, _ = a.Next()
		bk, _ = b.Next()
	}
	return nil
}
func compareCompactBuckets(source, destination *bolt.Bucket, path []string, options CompactOptions) error {
	if source == nil || destination == nil || source.Sequence() != destination.Sequence() {
		return errors.New("bucket sequence changed")
	}
	a, b := source.Cursor(), destination.Cursor()
	ak, av := a.First()
	bk, bv := b.First()
	for ak != nil || bk != nil {
		if !bytes.Equal(ak, bk) || (av == nil) != (bv == nil) {
			return errors.New("bucket entries changed")
		}
		if av == nil {
			if err := compareCompactBuckets(source.Bucket(ak), destination.Bucket(bk), append(path[:len(path):len(path)], string(ak)), options); err != nil {
				return err
			}
		} else {
			decodedSource, err := decodeCompactCredential(path, string(ak), av, options.credentials)
			if err != nil {
				return err
			}
			decodedTarget, err := decodeCompactCredential(path, string(bk), bv, options.credentials)
			if err != nil {
				return err
			}
			if options.Normalize != nil {
				decodedSource, err = options.Normalize(path, string(ak), decodedSource)
				if err != nil {
					return err
				}
				if decodedSource == nil {
					return errors.New("compact normalization cannot remove a record")
				}
			}
			if len(path) == 1 && path[0] == "metadata" && bytes.Equal(ak, []byte("format")) {
				expected, err := compactFormat(decodedSource)
				if err != nil || !bytes.Equal(expected, decodedTarget) {
					return errors.New("database format changed unexpectedly")
				}
			} else if !bytes.Equal(decodedSource, decodedTarget) {
				return errors.New("record changed during compaction")
			}
		}
		ak, av = a.Next()
		bk, bv = b.Next()
	}
	return nil
}

func compactFormat(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var version int
	if err := json.Unmarshal(fields["schemaVersion"], &version); err != nil {
		return nil, err
	}
	if !supportedSchema(version) {
		return nil, errors.New("cannot compact an unsupported database schema")
	}
	// Compaction changes record encoding, not task ownership semantics. Keep
	// newer reader requirements such as leases when rewriting the format row.
	fields["schemaVersion"] = json.RawMessage(fmt.Sprint(max(version, SchemaCompressedValues)))
	return json.Marshal(fields)
}

func decodeCompactCredential(path []string, key string, stored []byte, credentials *credentialCipher) ([]byte, error) {
	if len(path) == 1 && path[0] == credentialBucket && credentials != nil {
		var err error
		stored, err = credentials.open(credentialBucket, key, stored)
		if err != nil {
			return nil, err
		}
	}
	return decodeValue(stored)
}
