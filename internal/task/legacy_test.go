package task

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func pendingMigration(t *testing.T, db *state.Store) {
	t.Helper()
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put("migration", "state", map[string]any{"migrationId": "fixture", "pending": true})
	}); err != nil {
		t.Fatal(err)
	}
}
func legacyParent(id int64, status string, deleted bool) LegacyParentImport {
	return LegacyParentImport{Parent: Parent{ID: id, Spec: testSpec(), Generation: 1, BudgetRevision: 1, Status: status, CreatedAt: time.Unix(1700000000, 0).UTC(), Deleted: deleted, ChildCount: 1, Legacy: &LegacyProvenance{MigrationID: "fixture", OriginalStatus: "RUNNING", NeedsReconciliation: status != "COMPLETED", SourceNodes: []string{"old-node"}}}, Children: []LegacyChild{{ParentID: id, Number: 1, Node: "old-node", TaskID: -42, Key: "task:-42", Raw: json.RawMessage(`{"taskIndex":-42,"marker":"partial","putObjectCount":1}`)}}}
}
func TestLegacyImportIsOfflineAtomicIndexedAndFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	input := LegacyImport{MigrationID: "fixture", HighWater: 900, Parents: []LegacyParentImport{legacyParent(224, "STOPPED", false), legacyParent(300, "COMPLETED", true)}, PurgedIDs: []int64{800}}
	if err := repo.ImportLegacy(input); err == nil {
		t.Fatal("import into live database")
	}
	pendingMigration(t, db)
	invalid := input
	invalid.Parents = append(append([]LegacyParentImport(nil), input.Parents...), legacyParent(-1, "STOPPED", false))
	if err := repo.ImportLegacy(invalid); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, err := repo.Get(224); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed transaction retained first parent", err)
	}
	var next uint64
	if err := db.Update(func(tx *state.Tx) error { var e error; next, e = tx.NextID("master-tasks"); return e }); err != nil || next != 1 {
		t.Fatal("failed import advanced sequence", next, err)
	}
	for range 2 {
		if err := repo.ImportLegacy(input); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	active, err := repo.List(0, 100, true)
	if err != nil || len(active) != 0 {
		t.Fatal("legacy task was runnable", active, err)
	}
	visible, err := repo.Recent(0, 10, false)
	if err != nil || len(visible) != 1 || visible[0].ID != 224 {
		t.Fatal(visible, err)
	}
	recycled, err := repo.Recent(0, 10, true)
	if err != nil || len(recycled) != 1 || recycled[0].ID != 300 {
		t.Fatal(recycled, err)
	}
	p, err := repo.Get(800)
	if err != nil || !p.Purged || !p.Deleted {
		t.Fatal("lost purge fence", p, err)
	}
	if _, err := repo.Resume(224); !errors.Is(err, ErrLegacyRecovery) {
		t.Fatal("resumed without reconciliation", err)
	}
	children, err := repo.LegacyChildren(224, 0, 1)
	if err != nil || len(children) != 1 || children[0].TaskID != -42 {
		t.Fatal(children, err)
	}
	live, err := repo.Children(224, 0, 256)
	if err != nil || len(live) != 0 {
		t.Fatal("fabricated execution job", live, err)
	}
	created, _, err := repo.Create(testSpec(), "new-task")
	if err != nil || created.ID != 901 {
		t.Fatal("allocator reused old IDs", created.ID, err)
	}
	if err := db.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("pending guard removed", err)
	}
	changed := input
	changed.Parents = append([]LegacyParentImport(nil), input.Parents...)
	changed.Parents[0].Parent.Error = "changed"
	if err := repo.ImportLegacy(changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed import overwrote row", err)
	}
}
func TestLegacyHistoryRetainsResultDistinctionsAndPurgeRemovesIndexes(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	pendingMigration(t, db)
	if err := repo.ImportLegacy(LegacyImport{MigrationID: "fixture", Parents: []LegacyParentImport{legacyParent(224, "STOPPED", false)}}); err != nil {
		t.Fatal(err)
	}
	rows := []LegacyHistory{{ParentID: 224, ChildTaskID: "224-1", Node: "old-node", Key: "file-report:224:item:YQ", Kind: "OBJECT", Cursor: "a", Raw: json.RawMessage(`{"logicalKey":"a","result":"SUCCESS"}`)}, {ParentID: 224, Node: "old-node", Key: "file-report:224:item:Yg", Kind: "OBJECT", Cursor: "b", Raw: json.RawMessage(`{"logicalKey":"b","result":"UNPROCESSED"}`)}}
	for range 2 {
		if err := repo.PutLegacyHistory("fixture", rows); err != nil {
			t.Fatal(err)
		}
	}
	first, err := repo.LegacyHistory(224, "OBJECT", "", 1)
	if err != nil || len(first) != 1 || first[0].Cursor != "a" {
		t.Fatal(first, err)
	}
	second, err := repo.LegacyHistory(224, "OBJECT", first[0].Cursor, 1)
	if err != nil || len(second) != 1 || second[0].Cursor != "b" {
		t.Fatal(second, err)
	}
	changed := rows[0]
	changed.Raw = json.RawMessage(`{"result":"FAILED"}`)
	if err := repo.PutLegacyHistory("fixture", []LegacyHistory{changed}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := repo.Recycle(224, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Purge(224); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LegacyHistory(224, "OBJECT", "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatal("purged history visible", err)
	}
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan("master-task-legacy-history", "", "", 1, func(string, json.RawMessage) error { t.Fatal("indexed purged history retained"); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	// A replayed old worker report cannot resurrect a permanently deleted report.
	if err := repo.PutLegacyHistory("fixture", rows); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyChildPaginationUsesRowsWhenHistoricalNumbersHaveGaps(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	pendingMigration(t, db)
	row := legacyParent(224, "STOPPED", false)
	other := row.Children[0]
	other.Number = 9
	other.TaskID = -43
	other.Key = "task:-43"
	row.Children = append(row.Children, other)
	row.Parent.ChildCount = 2
	if err := repo.ImportLegacy(LegacyImport{MigrationID: "fixture", Parents: []LegacyParentImport{row}}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.LegacyChildrenPage(224, 0, 1)
	if err != nil || len(first) != 1 || first[0].Number != 1 {
		t.Fatal(first, err)
	}
	second, err := repo.LegacyChildrenPage(224, 1, 1)
	if err != nil || len(second) != 1 || second[0].Number != 9 {
		t.Fatal(second, err)
	}
	done, err := repo.LegacyChildrenPage(224, 2, 1)
	if err != nil || len(done) != 0 {
		t.Fatal(done, err)
	}
}

func TestLegacyChildNumberUsesSameSigned32BitBoundsOnEveryArchitecture(t *testing.T) {
	for _, number := range []int{0, -1, math.MaxInt32} {
		t.Run(strconv.Itoa(number), func(t *testing.T) {
			db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
			defer db.Close()
			pendingMigration(t, db)
			row := legacyParent(224, "COMPLETED", false)
			row.Children[0].Number = number
			err := repo.ImportLegacy(LegacyImport{MigrationID: "fixture", Parents: []LegacyParentImport{row}})
			if number <= 0 {
				if err == nil {
					t.Fatal("invalid child number imported")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			children, err := repo.LegacyChildrenPage(224, 0, 1)
			if err != nil || len(children) != 1 || children[0].Number != number {
				t.Fatal("boundary child number changed after import", children, err)
			}
		})
	}
	if strconv.IntSize == 64 {
		db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
		defer db.Close()
		pendingMigration(t, db)
		row := legacyParent(224, "COMPLETED", false)
		overflow := int64(math.MaxInt32) + 1
		row.Children[0].Number = int(overflow)
		if err := repo.ImportLegacy(LegacyImport{MigrationID: "fixture", Parents: []LegacyParentImport{row}}); err == nil {
			t.Fatal("child number outside the portable range imported")
		}
	}
}
