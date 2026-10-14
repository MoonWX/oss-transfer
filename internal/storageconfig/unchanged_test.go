package storageconfig

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotReplayDoesNotCommitAndUpdatesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db := openTestStore(t, path)
	agent := NewAgent(db)
	digest := func() [32]byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	unchanged := func(snapshot Snapshot, expected error) {
		t.Helper()
		before := digest()
		applied, err := agent.ApplySnapshot(snapshot)
		if applied || !errors.Is(err, expected) {
			t.Fatalf("unexpected replay result: %v %v", applied, err)
		}
		if digest() != before {
			t.Fatal("snapshot replay changed on-disk database, including metadata")
		}
	}
	unchanged(Snapshot{}, nil)
	snapshot := Snapshot{Revision: 1, Connections: []Config{testConfig("source"), testConfig("target")}}
	if applied, err := agent.ApplySnapshot(snapshot); err != nil || !applied {
		t.Fatal(applied, err)
	}
	unchanged(snapshot, nil)
	unchanged(Snapshot{Revision: 1, Connections: []Config{snapshot.Connections[1], snapshot.Connections[0]}}, nil)
	unchanged(Snapshot{}, ErrStaleRevision)
	conflict := Snapshot{Revision: 1, Connections: []Config{testConfig("different")}}
	unchanged(conflict, ErrRevisionConflict)
	snapshot = Snapshot{Revision: 2, Connections: []Config{testConfig("target")}}
	if applied, err := agent.ApplySnapshot(snapshot); err != nil || !applied {
		t.Fatal(applied, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestStore(t, path)
	agent = NewAgent(db)
	assertSnapshot(t, agent, snapshot)
	unchanged(snapshot, nil)
	unchanged(conflict, ErrStaleRevision)
}
