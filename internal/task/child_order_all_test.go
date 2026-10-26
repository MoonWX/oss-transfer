package task

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestAllChildOrderGlobalPagesStateChangesAndDowngradeRebuild(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "all-priority")
	if err != nil {
		t.Fatal(err)
	}
	var children []Child
	var observations []Observation
	now := time.Now().UTC()
	for number := 1; number <= 300; number++ {
		child, err := repo.Assign(p.ID, p.Generation, number, objects(fmt.Sprint(number)))
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
		status := execution.Completed
		switch number {
		case 291, 292:
			status = execution.Failed
		case 293:
			status = execution.PartialFailed
		case 294:
			status = execution.Recoverable
		case 295:
			status = execution.Running
		case 296:
			status = "RESUMING"
		case 297:
			status = "STOPPING"
		case 298:
			status = execution.Waiting
		case 299:
			status = "STOPPED"
		case 300:
			status = execution.Deleted
		}
		observations = append(observations, Observation{Number: number, Record: execution.Record{Job: child.Job, Status: status, UpdatedAt: now, Attempt: 1}})
	}
	for start := 0; start < len(observations); start += 200 {
		if err := repo.ObserveBatch(p.ID, p.Generation, observations[start:min(start+200, len(observations))]); err != nil {
			t.Fatal(err)
		}
	}
	page := func(after string, known ...int) ([]Child, []Child) {
		t.Helper()
		for attempt := 0; attempt < 10; attempt++ {
			rows, updates, ready, err := repo.PriorityChildren(context.Background(), p.ID, after, 2, known)
			if err != nil {
				t.Fatal(err)
			}
			if ready {
				return rows, updates
			}
		}
		t.Fatal("bounded index did not converge")
		return nil, nil
	}
	numbers := func(rows []Child) []int {
		result := []int{}
		for _, row := range rows {
			result = append(result, row.Job.Number)
		}
		return result
	}
	first, _ := page("")
	if !reflect.DeepEqual(numbers(first), []int{291, 292}) {
		t.Fatal(numbers(first))
	}
	second, _ := page(AllChildOrderCursor(first[1]))
	if !reflect.DeepEqual(numbers(second), []int{293, 294}) {
		t.Fatal(numbers(second))
	}
	third, _ := page(AllChildOrderCursor(second[1]))
	if !reflect.DeepEqual(numbers(third), []int{295, 296}) {
		t.Fatal(numbers(third))
	}
	// The first completed child is far earlier numerically, yet comes after
	// every unfinished or failed child across independently loaded pages.
	all := numbers(first)
	after := AllChildOrderCursor(first[len(first)-1])
	for {
		rows, _ := page(after)
		if len(rows) == 0 {
			break
		}
		all = append(all, numbers(rows)...)
		after = AllChildOrderCursor(rows[len(rows)-1])
	}
	if len(all) != 300 || all[9] != 1 || all[len(all)-1] != 300 {
		t.Fatalf("incorrect full ordering: count=%d first=%v last=%v", len(all), all[:12], all[len(all)-3:])
	}
	seen := map[int]bool{}
	for _, n := range all {
		if seen[n] {
			t.Fatalf("duplicate child %d", n)
		}
		seen[n] = true
	}
	if err := repo.Observe(p.ID, p.Generation, 298, execution.Record{Job: children[297].Job, Status: execution.Running, UpdatedAt: now.Add(time.Second), Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	rows, updates := page("02/0000000294", 298)
	if !reflect.DeepEqual(numbers(rows), []int{295, 298}) || len(updates) != 1 || childStatus(updates[0]) != execution.Running {
		t.Fatal(numbers(rows), updates)
	}
	old, _ := repo.Get(p.ID)
	oldRevision := old.ChildOrder.All.Revision
	// Simulate a v4.1.16 writer changing a child without maintaining the new
	// all-state index, and dropping its unknown metadata field on parent save.
	if err := db.Update(func(tx *state.Tx) error {
		parent, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		parent.ChildOrder.All = nil
		child := children[290]
		child.Record = &execution.Record{Job: child.Job, Status: execution.Completed, UpdatedAt: now.Add(2 * time.Second), Attempt: 2}
		if err := tx.Put("master-task-children", childKey(p.ID, 291), child); err != nil {
			return err
		}
		return save(tx, parent)
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = page("")
	if !reflect.DeepEqual(numbers(rows), []int{292, 293}) {
		t.Fatal("stale index visible", numbers(rows))
	}
	current, _ := repo.Get(p.ID)
	if current.ChildOrder.All.Revision == oldRevision {
		t.Fatal("reused stale index revision")
	}
	// The original active index must not include entries from the all index.
	active, _, err := repo.ActiveChildren(p.ID, "", 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range active {
		if childPriority(child) < 0 {
			t.Fatalf("terminal child leaked into active view: %d", child.Job.Number)
		}
	}
}

func TestAllChildIndexBuildIsBoundedDurableAndTracksConcurrentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { db.Close() }()
	p, _, err := repo.Create(testSpec(), "bounded-all-priority")
	if err != nil {
		t.Fatal(err)
	}
	// Populate a realistic persisted assignment stream in one test fixture
	// transaction; the production migration must never process it all at once.
	if err := db.Update(func(tx *state.Tx) error {
		parent, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		parent.ChildCount, parent.DispatchedThrough = 2100, 2100
		parent.ChildOrder = &ChildOrderState{Version: 1}
		for number := 1; number <= 2100; number++ {
			child := Child{Job: execution.Job{ParentID: p.ID, Number: number, ChildID: fmt.Sprintf("%d-%d", p.ID, number), Generation: p.Generation}}
			child.Record = &execution.Record{Job: child.Job, Status: execution.Completed}
			if err := tx.Put("master-task-children", childKey(p.ID, number), child); err != nil {
				return err
			}
		}
		return save(tx, parent)
	}); err != nil {
		t.Fatal(err)
	}
	step := func() bool {
		t.Helper()
		var ready bool
		if err := db.Update(func(tx *state.Tx) error {
			parent, err := get(tx, p.ID)
			if err != nil {
				return err
			}
			ready, err = prepareAllChildOrderPage(tx, &parent)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	}
	if step() {
		t.Fatal("large index rebuilt in one write transaction")
	}
	parent, _ := repo.Get(p.ID)
	if parent.ChildOrder.All.After != childKey(p.ID, allChildOrderPage) {
		t.Fatal(parent.ChildOrder.All)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	if step() {
		t.Fatal("index unexpectedly complete after restart")
	}
	parent, _ = repo.Get(p.ID)
	if parent.ChildOrder.All.After != childKey(p.ID, 2*allChildOrderPage) {
		t.Fatal("restart lost durable cursor", parent.ChildOrder.All)
	}
	// State changes on both sides of the build cursor must be reflected once.
	if err := db.Update(func(tx *state.Tx) error {
		parent, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		for _, n := range []int{10, 2000} {
			var old Child
			if _, err := tx.Get("master-task-children", childKey(p.ID, n), &old); err != nil {
				return err
			}
			next := old
			record := *old.Record
			record.Status = execution.Failed
			next.Record = &record
			if err := tx.Put("master-task-children", childKey(p.ID, n), next); err != nil {
				return err
			}
			if err := syncChildOrder(tx, &parent, &old, next); err != nil {
				return err
			}
		}
		return save(tx, parent)
	}); err != nil {
		t.Fatal(err)
	}
	for !step() {
	}
	rows, _, ready, err := repo.PriorityChildren(context.Background(), p.ID, "", 3, nil)
	if err != nil || !ready || len(rows) != 3 || rows[0].Job.Number != 10 || rows[1].Job.Number != 2000 || rows[2].Job.Number != 1 {
		t.Fatal(rows, ready, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := repo.PriorityChildren(ctx, p.ID, "", 3, nil); err != context.Canceled {
		t.Fatal("ignored cancellation", err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		parent, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		parent.Status = "COMPLETED"
		parent.Deleted = true
		return save(tx, parent)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Purge(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(allChildOrderBucket, key(p.ID)+"/", "", 1, func(string, json.RawMessage) error { t.Error("purge retained all-state index"); return nil })
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAllChildCursorValidation(t *testing.T) {
	for _, bad := range []string{"0/0000000001", "11/0000000001", "00/0", "00/0000000000", "../0000000001", "00/999999999999999999999"} {
		if ValidAllChildOrderCursor(bad) {
			t.Fatal("invalid cursor", bad)
		}
	}
	for _, good := range []string{"00/0000000001", "09/0000000100", "10/0000000001"} {
		if !ValidAllChildOrderCursor(good) {
			t.Fatal("valid cursor rejected", good)
		}
	}
}
