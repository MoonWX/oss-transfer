package state

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompressedRecordsKeepOldJSONAndAllReadPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	value := map[string]string{"object": "目录/" + strings.Repeat("training-data/", 1000)}
	plain, _ := json.Marshal(value)
	if err := db.Update(func(tx *Tx) error { return tx.Put("records", "old", json.RawMessage(plain)) }); err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error { return tx.Put("records", "new", value) }); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error {
		b := tx.tx.Bucket([]byte("records"))
		if !bytes.Equal(b.Get([]byte("old")), plain) {
			t.Fatal("historical row was rewritten")
		}
		stored := b.Get([]byte("new"))
		if len(stored) >= len(plain)/3 || !bytes.HasPrefix(stored, compressedMagic) {
			t.Fatalf("record was not compressed: %d/%d", len(stored), len(plain))
		}
		if !bytes.Contains(tx.tx.Bucket([]byte("metadata")).Get([]byte("format")), []byte(`"schemaVersion":4`)) {
			t.Fatal("old-reader fence missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := transactionID(t, db)
	if err := db.Update(func(tx *Tx) error { return tx.Put("records", "new", value) }); err != nil {
		t.Fatal(err)
	}
	if transactionID(t, db) != before {
		t.Fatal("identical compressed write created a new commit")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error {
		for _, key := range []string{"old", "new"} {
			var got map[string]string
			found, err := tx.Get("records", key, &got)
			if err != nil || !found || !reflect.DeepEqual(got, value) {
				t.Fatalf("get %s: %v", key, err)
			}
		}
		for _, reverse := range []bool{false, true} {
			var retained []json.RawMessage
			visit := func(_ string, raw json.RawMessage) error { retained = append(retained, raw); return nil }
			var err error
			if reverse {
				err = tx.ScanReverse("records", "", 10, visit)
			} else {
				err = tx.Scan("records", "", "", 10, visit)
			}
			if err != nil || len(retained) != 2 {
				t.Fatalf("scan %t: %v", reverse, err)
			}
			for _, raw := range retained {
				if !bytes.Equal(raw, plain) {
					t.Fatal("scan returned compressed or altered API JSON")
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompressionFenceAndDataRollbackTogether(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *Tx) error {
		return tx.Put("metadata", "format", databaseFormat{Role: "agent", SchemaVersion: 1})
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort")
	if err := db.Update(func(tx *Tx) error {
		if err := RequireSchema(tx, "agent", SchemaCompressedValues); err != nil {
			return err
		}
		if err := tx.Put("records", "new", strings.Repeat("compressible", 1000)); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if readFormat(t, db).SchemaVersion != 1 {
		t.Fatal("aborted transaction raised reader requirement")
	}
	if err := db.View(func(tx *Tx) error {
		var value any
		found, err := tx.Get("records", "new", &value)
		if found {
			t.Fatal("aborted compressed record survived")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedFrameRejectsCorruptionAndExpansion(t *testing.T) {
	raw := []byte(`{"data":"` + strings.Repeat("abc", 4000) + `"}`)
	valid, err := encodeValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]byte) []byte{
		"magic":        func(b []byte) []byte { b[1] = 'X'; return b },
		"codec":        func(b []byte) []byte { b[6] = 2; return b },
		"short-header": func(b []byte) []byte { return b[:10] },
		"truncated":    func(b []byte) []byte { return b[:len(b)-2] },
		"checksum":     func(b []byte) []byte { b[16] ^= 1; return b },
		"short-length": func(b []byte) []byte { binary.BigEndian.PutUint64(b[8:16], 512); return b },
		"long-length":  func(b []byte) []byte { binary.BigEndian.PutUint64(b[8:16], uint64(len(raw)+1)); return b },
		"huge-length":  func(b []byte) []byte { binary.BigEndian.PutUint64(b[8:16], 1<<63); return b },
		"trailing":     func(b []byte) []byte { return append(b, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeValue(change(bytes.Clone(valid))); !errors.Is(err, ErrCorruptValue) {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
	if got, err := decodeValue(valid); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("valid frame changed", err)
	}
}

func TestCorruptCompressedRecordFailsEveryReadPath(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	frame, _ := encodeValue([]byte(`"` + strings.Repeat("content", 1000) + `"`))
	frame[16] ^= 1
	if err := db.Update(func(tx *Tx) error {
		b, err := tx.tx.CreateBucket([]byte("records"))
		if err != nil {
			return err
		}
		tx.dirty = true
		return b.Put([]byte("bad"), frame)
	}); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(*Tx) error{
		func(tx *Tx) error { var got string; _, err := tx.Get("records", "bad", &got); return err },
		func(tx *Tx) error {
			return tx.Scan("records", "", "", 10, func(string, json.RawMessage) error { t.Fatal("corrupt value reached caller"); return nil })
		},
		func(tx *Tx) error {
			return tx.ScanReverse("records", "", 10, func(string, json.RawMessage) error { t.Fatal("corrupt value reached caller"); return nil })
		},
	} {
		if err := db.View(read); !errors.Is(err, ErrCorruptValue) {
			t.Fatal("corrupt record not rejected", err)
		}
	}
}

func TestClaimRoleRejectsFutureSchemaWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *Tx) error {
		return tx.Put("metadata", "format", databaseFormat{Role: "master", SchemaVersion: SchemaCompressedValues + 1})
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := db.ClaimRole("master"); err == nil {
		t.Fatal("future schema accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed claim modified future database")
	}
}
