package task

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestBeginLegacyRecoveryAtomicAndIdempotent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, file)
	pendingMigration(t, db)
	old := legacyParent(224, "STOPPED", false)
	old.Parent.ScannedObjects = 91
	old.Parent.Counters.Succeeded = 90
	if err := repo.ImportLegacy(LegacyImport{MigrationID: "fixture", Parents: []LegacyParentImport{old}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BeginLegacyRecovery(224, old.Parent.Spec); err == nil {
		t.Fatal("manifest not ready")
	}
	if err := repo.ReadyLegacyRecovery(224, "fixture"); err != nil {
		t.Fatal(err)
	}
	bad := old.Parent.Spec
	bad.ConflictPolicy = "SKIP"
	if old.Parent.Spec.ConflictPolicy == "SKIP" {
		bad.ConflictPolicy = "OVERWRITE"
	}
	if _, err := repo.BeginLegacyRecovery(224, bad); err == nil {
		t.Fatal("scope altered")
	}
	p, _ := repo.Get(224)
	if p.Generation != 1 || p.Counters.Succeeded != 90 {
		t.Fatal("failed transaction changed old task", p)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := repo.BeginLegacyRecovery(224, old.Parent.Spec)
			if err != nil || p.Generation != 2 || p.Status != "RESUMING" {
				t.Error(p, err)
			}
		}()
	}
	wg.Wait()
	if err := db.View(func(tx *state.Tx) error {
		var saved Parent
		found, err := tx.Get("master-task-legacy-original", key(224), &saved)
		if !found || saved.Generation != 1 || saved.Counters.Succeeded != 90 {
			t.Error("original observation not retained", saved)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Resumed(224, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Stop(224); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, file)
	defer db.Close()
	p, err := repo.BeginLegacyRecovery(224, old.Parent.Spec)
	if err != nil || p.Generation != 2 || p.Status != "STOPPING" || p.Counters.Succeeded != 0 {
		t.Fatal("uncertain retry reset or resurrected work", p, err)
	}
	children, err := repo.LegacyChildren(224, 0, 128)
	if err != nil || len(children) != 1 {
		t.Fatal("original child history lost", children, err)
	}
}
