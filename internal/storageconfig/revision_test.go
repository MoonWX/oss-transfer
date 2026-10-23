package storageconfig

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestSettingsEditCannotRestoreCredentialsAfterConcurrentRotation(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "connections.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := NewMaster(db)
	initial := testConfig("source")
	previous, err := m.Create(initial)
	if err != nil {
		t.Fatal(err)
	}
	rotated := initial
	rotated.SecretKey = "new fixture credential"
	if _, err := m.Update("source", rotated); err != nil {
		t.Fatal(err)
	}
	initial.Region = "new-region"
	if _, err := m.UpdateAtRevision("source", initial, previous.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale edit should fail: %v", err)
	}
	current, err := m.Snapshot()
	if err != nil || current.Connections[0].SecretKey != rotated.SecretKey {
		t.Fatal("stale edit reverted the newer credential")
	}
}
