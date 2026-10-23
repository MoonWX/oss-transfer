package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestCompactOfflineShrinksAndPreservesRecordsNestedBucketsAndSequences(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		t.Run(role, func(t *testing.T) {
			path, _ := restoreFixture(t, role)
			value := map[string]string{"source": strings.Repeat("training-object-", 500)}
			raw, _ := json.Marshal(value)
			mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
				b, err := tx.CreateBucket([]byte("large"))
				if err != nil {
					return err
				}
				for i := 0; i < 1200; i++ {
					if err := b.Put([]byte(fmt.Sprintf("%08d", i)), raw); err != nil {
						return err
					}
				}
				// Metadata unknown to this version must survive too.
				return tx.Bucket([]byte("metadata")).Put([]byte("format"), []byte(fmt.Sprintf(`{"role":%q,"schemaVersion":1,"extension":"retained"}`, role)))
			})
			before := fileBytes(t, path)
			output := filepath.Join(filepath.Dir(path), "compact.db")
			var progress bytes.Buffer
			if err := CompactOfflineWithProgress(path, role, output, &progress); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("compaction modified source")
			}
			if len(fileBytes(t, output)) >= len(before)/4 {
				t.Fatalf("poor compaction: %d -> %d", len(before), len(fileBytes(t, output)))
			}
			if !strings.Contains(progress.String(), "Verifying compacted database") || !strings.Contains(progress.String(), "compaction complete") {
				t.Fatal("missing progress", progress.String())
			}
			if strings.Contains(progress.String(), "qa-local") || strings.Contains(progress.String(), "training-object") {
				t.Fatal("progress exposed records")
			}
			original, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			copied, err := bolt.Open(output, 0600, &bolt.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer copied.Close()
			if err := original.View(func(a *bolt.Tx) error {
				return copied.View(func(b *bolt.Tx) error { return compareCompactTransactions(a, b) })
			}); err != nil {
				t.Fatal(err)
			}
			if err := copied.View(func(tx *bolt.Tx) error {
				if !bytes.Contains(tx.Bucket([]byte("metadata")).Get([]byte("format")), []byte(`"extension":"retained"`)) {
					t.Fatal("metadata extension lost")
				}
				stored := tx.Bucket([]byte("large")).Get([]byte("00000000"))
				if !bytes.HasPrefix(stored, compressedMagic) {
					t.Fatal("legacy row not compressed")
				}
				got, err := decodeValue(stored)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatal("compressed JSON changed", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if found, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".compact.db.compact-*")); len(found) != 0 {
				t.Fatal("temporary file remains", found)
			}
		})
	}
}

func TestCompactOfflineRejectsRunningSourceAndExistingDestination(t *testing.T) {
	path, _ := restoreFixture(t, "master")
	output := filepath.Join(filepath.Dir(path), "compact.db")
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := fileBytes(t, path)
	if err := CompactOffline(path, "master", output); !errors.Is(err, bolt.ErrTimeout) {
		t.Fatal("active writer accepted", err)
	}
	if !bytes.Equal(original, fileBytes(t, path)) {
		t.Fatal("failed compaction changed live source")
	}
	live.Close()
	if err := os.WriteFile(output, []byte("existing snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CompactOffline(path, "master", output); err == nil {
		t.Fatal("existing destination replaced")
	}
	if !bytes.Equal(fileBytes(t, output), []byte("existing snapshot")) {
		t.Fatal("existing destination modified")
	}
	if err := CompactOffline(path, "master", path); err == nil {
		t.Fatal("source replacement accepted")
	}
}

func TestCompactOfflineCorruptionCleansPartialOutput(t *testing.T) {
	path, _ := restoreFixture(t, "master")
	output := filepath.Join(filepath.Dir(path), "compact.db")
	mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("records"))
		if err != nil {
			return err
		}
		if err := b.Put([]byte("good"), []byte(`"`+strings.Repeat("long", 1000)+`"`)); err != nil {
			return err
		}
		bad, _ := encodeValue([]byte(`"` + strings.Repeat("long", 1000) + `"`))
		bad[16] ^= 1
		return b.Put([]byte("zz-bad"), bad)
	})
	before := fileBytes(t, path)
	if err := CompactOffline(path, "master", output); !errors.Is(err, ErrCorruptValue) {
		t.Fatal("corrupt source accepted", err)
	}
	if !bytes.Equal(before, fileBytes(t, path)) {
		t.Fatal("failed compaction changed source")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial final output exists", err)
	}
	if found, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".compact.db.compact-*")); len(found) != 0 {
		t.Fatal("partial temporary output exists", found)
	}
}

func TestCompactPublicationNeverOverwritesConcurrentCreator(t *testing.T) {
	dir := t.TempDir()
	temporary, destination := filepath.Join(dir, "temporary"), filepath.Join(dir, "destination")
	if err := os.WriteFile(temporary, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("winner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishCompact(temporary, destination); err == nil {
		t.Fatal("concurrent destination overwritten")
	}
	if string(fileBytes(t, destination)) != "winner" {
		t.Fatal("destination changed")
	}
}

func TestCompactSpaceReserveAndUncommittedFailure(t *testing.T) {
	const pending = 8 << 20
	need := uint64(compactSpaceReserve + 32<<20 + 2*pending)
	if err := checkCompactSpace(need, pending); err != nil {
		t.Fatal(err)
	}
	if err := checkCompactSpace(need-1, pending); !errors.Is(err, ErrCompactSpace) {
		t.Fatal("reserve was not enforced", err)
	}
	path := filepath.Join(t.TempDir(), "new.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before := fileBytes(t, path)
	writer := compactWriter{db: db, available: func() (uint64, error) { return 0, nil }}
	b, err := writer.destination([][]byte{[]byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	writer.bytes = pending
	if err := writer.commit(); !errors.Is(err, ErrCompactSpace) {
		t.Fatal("low-space commit accepted", err)
	}
	if writer.tx == nil {
		t.Fatal("failed space check lost rollback transaction")
	}
	if err := writer.tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, fileBytes(t, path)) {
		t.Fatal("low-space check wrote pending transaction")
	}
}

func TestCompactNormalizesKnownRecordsAndChecksResult(t *testing.T) {
	for _, scenario := range []string{"valid", "nondeterministic", "drop"} {
		t.Run(scenario, func(t *testing.T) {
			path, _ := restoreFixture(t, "master")
			output := filepath.Join(filepath.Dir(path), "normalized.db")
			mutateRestoreFile(t, path, func(tx *bolt.Tx) error {
				b, err := tx.CreateBucket([]byte("legacy"))
				if err != nil {
					return err
				}
				return b.Put([]byte("record"), []byte(`{"sameIdentity":"kept","redundant":0}`))
			})
			before := fileBytes(t, path)
			calls := 0
			options := CompactOptions{Normalize: func(path []string, key string, raw []byte) ([]byte, error) {
				if len(path) != 1 || path[0] != "legacy" || key != "record" {
					return raw, nil
				}
				calls++
				if scenario == "drop" {
					return nil, nil
				}
				if scenario == "nondeterministic" && calls > 1 {
					return []byte(`{"sameIdentity":"different"}`), nil
				}
				return []byte(`{"sameIdentity":"kept"}`), nil
			}}
			err := CompactOfflineWithOptions(path, "master", output, options)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatal("normalizer not independently checked", calls)
				}
			} else {
				if err == nil {
					t.Fatal("invalid normalization accepted")
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid normalized output published", err)
				}
			}
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("normalization modified source")
			}
		})
	}
}
