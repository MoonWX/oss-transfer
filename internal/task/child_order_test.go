package task

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestActiveChildOrderPagesTransitionsAndRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "priority")
	if err != nil {
		t.Fatal(err)
	}
	var children []Child
	var observations []Observation
	now := time.Now().UTC()
	for number := 1; number <= 205; number++ {
		child, err := repo.Assign(p.ID, p.Generation, number, objects(fmt.Sprint(number)))
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
		status := execution.Recoverable
		if number > 200 {
			status = execution.Running
		}
		observations = append(observations, Observation{Number: number, Record: execution.Record{Job: child.Job, Status: status, UpdatedAt: now, Attempt: 1}})
	}
	if err := repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
		t.Fatal(err)
	}
	page := func(after string, limit int, known ...int) ([]Child, []Child) {
		t.Helper()
		rows, updates, err := repo.ActiveChildren(p.ID, after, limit, known)
		if err != nil {
			t.Fatal(err)
		}
		return rows, updates
	}
	numbers := func(rows []Child) []int {
		var values []int
		for _, c := range rows {
			values = append(values, c.Job.Number)
		}
		return values
	}
	first, _ := page("", 2)
	if !reflect.DeepEqual(numbers(first), []int{201, 202}) {
		t.Fatal(numbers(first))
	}
	next, _ := page(ChildOrderCursor(first[1]), 4)
	if !reflect.DeepEqual(numbers(next), []int{203, 204, 205, 1}) {
		t.Fatal(numbers(next))
	}
	all, err := repo.Children(p.ID, 0, 3)
	if err != nil || !reflect.DeepEqual(numbers(all), []int{1, 2, 3}) {
		t.Fatal(numbers(all), err)
	}

	// A visible recoverable child moves ahead of its former cursor, beyond the
	// first page. It must still return in the bounded identity refresh.
	if err := repo.Observe(p.ID, p.Generation, 100, execution.Record{Job: children[99].Job, Status: execution.Running, UpdatedAt: now.Add(time.Second), Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Observe(p.ID, p.Generation, 201, finished(children[200])); err != nil {
		t.Fatal(err)
	}
	_, updates := page("3/0000000090", 2, 100, 201)
	if len(updates) != 2 || updates[0].Record.Status != execution.Running || updates[1].Record.Status != execution.Completed {
		t.Fatal(updates)
	}
	first, _ = page("", 2)
	if !reflect.DeepEqual(numbers(first), []int{100, 202}) {
		t.Fatal(numbers(first))
	}
	before, _ := repo.Get(p.ID)
	// Counter-only observations retain the same ordering keys and metadata.
	record := *first[0].Record
	record.UpdatedAt = now.Add(2 * time.Second)
	if err := repo.Observe(p.ID, p.Generation, 100, record); err != nil {
		t.Fatal(err)
	}
	after, _ := repo.Get(p.ID)
	if !reflect.DeepEqual(before.ChildOrder, after.ChildOrder) {
		t.Fatal("counter-only observation changed order state")
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	first, _ = page("", 2)
	if !reflect.DeepEqual(numbers(first), []int{100, 202}) {
		t.Fatal("restart changed priority", numbers(first))
	}
	// Simulate an older binary saving a parent and a child without maintaining
	// the newer derived index. Its parent encoding drops the unknown field.
	if err := db.Update(func(tx *state.Tx) error {
		parent, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		parent.ChildOrder = nil
		child := children[99]
		child.Record = &execution.Record{Job: child.Job, Status: execution.Waiting, Attempt: 2}
		if err := tx.Put("master-task-children", childKey(p.ID, 100), child); err != nil {
			return err
		}
		return tx.Put("master-tasks", key(p.ID), parent)
	}); err != nil {
		t.Fatal(err)
	}
	first, _ = page("", 2)
	if !reflect.DeepEqual(numbers(first), []int{202, 203}) {
		t.Fatal("stale downgrade index", numbers(first))
	}

	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	resumed, err := repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, _ = page("", 2)
	if !reflect.DeepEqual(numbers(first), []int{1, 2}) || childStatus(first[0]) != execution.Waiting {
		t.Fatal("resume did not reindex waiting children", numbers(first))
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, resumed.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Recycle(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Purge(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(childOrderBucket, key(p.ID)+"/", "", 1, func(_ string, _ json.RawMessage) error {
			t.Fatal("purge retained child ordering index")
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveChildPriorityKnownStates(t *testing.T) {
	for rank, status := range []execution.Status{execution.Running, "RECOVERING", "STOPPING", execution.Recoverable, execution.Waiting} {
		if actual := childPriority(Child{Record: &execution.Record{Status: status}}); actual != rank {
			t.Fatal(status, actual, rank)
		}
	}
	for _, cursor := range []string{"", "../0000000001", "0/0000000000", "6/0000000001", "0/999999999999999999999"} {
		if ValidChildOrderCursor(cursor) {
			t.Fatal("invalid cursor accepted", cursor)
		}
	}
}

func TestUnchangedObservationPersistsRebuiltChildOrder(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	parent, _, err := repo.Create(testSpec(), "unchanged-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Assign(parent.ID, parent.Generation, 1, objects("one"))
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: child.Job, Status: execution.Recoverable, Attempt: 1, UpdatedAt: time.Now().UTC()}
	observation := []Observation{{Number: 1, Record: record}}
	if err := repo.ObserveBatch(parent.ID, parent.Generation, observation); err != nil {
		t.Fatal(err)
	}
	// The old binary preserved the child ledger but knew nothing about this
	// derived index. Its next observation is byte-for-byte unchanged.
	if err := db.Update(func(tx *state.Tx) error {
		stored, err := get(tx, parent.ID)
		if err != nil {
			return err
		}
		stored.ChildOrder = nil
		if err := tx.Put("master-tasks", key(parent.ID), stored); err != nil {
			return err
		}
		return tx.Delete(childOrderBucket, key(parent.ID)+"/3/0000000001")
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ObserveBatch(parent.ID, parent.Generation, observation); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(parent.ID)
	if err != nil || stored.ChildOrder == nil || stored.ChildOrder.Version != 1 {
		t.Fatal("unchanged observation rolled back the derived index marker", stored.ChildOrder, err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var number int
		found, err := tx.Get(childOrderBucket, key(parent.ID)+"/3/0000000001", &number)
		if err != nil || !found || number != 1 {
			t.Fatal("rebuilt ordering key was not committed", found, number, err)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
