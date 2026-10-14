package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestMigrationFenceRejectsPendingOrMalformedRecordsForBothRoles(t *testing.T) {
	for _, role := range []string{"master", "agent"} {
		for _, record := range []any{nil, map[string]any{}, map[string]any{"pending": false}, map[string]any{"pending": true, "migrationId": "migration-1"}} {
			db, err := Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Update(func(tx *Tx) error { return tx.Put("migration", "state", record) }); err != nil {
				t.Fatal(err)
			}
			if err = db.ClaimRole(role); !errors.Is(err, ErrMigrationPending) {
				t.Fatalf("role=%s record=%#v escaped fence: %v", role, record, err)
			}
			db.Close()
		}
	}
}
