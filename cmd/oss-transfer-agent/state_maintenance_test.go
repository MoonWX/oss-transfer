package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestAgentCompactConvertsOldIndexAndKeepsIdentityAndSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "training/中文 + %/object.bin"
	indexKey := "00000000000000000001/" + base64.RawURLEncoding.EncodeToString([]byte(key))
	object := transferstore.Object{Key: key, Size: 4096, ModifiedAt: time.Unix(1720000000, 1234).UTC(), ETag: "source-identity", Metadata: map[string]string{"owner": "retained"}}
	legacy, err := json.Marshal(map[string]any{"object": object, "seenAttempt": "scan-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	identity := map[string]string{"instanceId": "test-node", "credential": "test-only-credential", "enrollmentId": "test-enrollment", "masterUrl": "https://test.example"}
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put("metadata", "format", map[string]any{"role": "agent", "schemaVersion": 1}); err != nil {
			return err
		}
		if err := tx.Put("agent-identity", "self", identity); err != nil {
			return err
		}
		return tx.Put("discovery-objects", indexKey, json.RawMessage(legacy))
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
	destination := filepath.Join(filepath.Dir(path), "compact.db")
	if err := compactAgentDatabase(path, destination, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("compaction changed the source database", err)
	}
	copy, err := state.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if err := copy.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	if err := copy.View(func(tx *state.Tx) error {
		var gotIdentity map[string]string
		found, err := tx.Get("agent-identity", "self", &gotIdentity)
		if err != nil || !found || len(gotIdentity) != len(identity) {
			t.Fatalf("lost agent identity: found=%v err=%v", found, err)
		}
		for key, value := range identity {
			if gotIdentity[key] != value {
				t.Fatalf("changed identity field %s", key)
			}
		}
		var row json.RawMessage
		found, err = tx.Get("discovery-objects", indexKey, &row)
		if err != nil || !found {
			t.Fatalf("lost index entry: found=%v err=%v", found, err)
		}
		want, err := discovery.CompactObjectIndexValue(indexKey, legacy)
		if err != nil || !bytes.Equal(row, want) || len(row) >= len(legacy) {
			t.Fatalf("old index was not normalized: before=%d after=%d err=%v", len(legacy), len(row), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
