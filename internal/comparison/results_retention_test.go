package comparison

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func sourceOf(n int) *fakeStore {
	var objects []transferstore.Object
	for i := 0; i < n; i++ {
		objects = append(objects, object(fmt.Sprintf("src/%04d", i), 1))
	}
	return newFakeStore(objects...)
}

func listedIDs(t *testing.T, m *Manager) []string {
	t.Helper()
	items, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range items {
		ids = append(ids, p.ID)
	}
	return ids
}

// assertGone checks that nothing of a comparison is left.
func assertGone(t *testing.T, h *harness, m *Manager, id string) {
	t.Helper()
	if _, err := m.Get(id, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%s still readable: %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, resultDirectory(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s results directory left: %v", id, err)
	}
	for _, bucket := range []string{runsBucket, legacyBucket} {
		if h.exists(bucket, id) {
			t.Fatalf("%s record left in %s", id, bucket)
		}
	}
	if h.count(blocksBucket, id+"/") != 0 || h.count(legacyRowsBucket, id+"/") != 0 {
		t.Fatalf("%s rows left", id)
	}
}

// seedLegacy writes a comparison the way Agents before Protocol 2 did.
func seedLegacy(h *harness, id string, p Progress, rows []Row) {
	h.t.Helper()
	p.Request = requestWithID(id)
	p.ResultLimit = LegacyStoredDifferences
	p.Rows = int64(len(rows))
	if err := h.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(legacyBucket, id, p); err != nil {
			return err
		}
		for _, row := range rows {
			if err := tx.Put(legacyRowsBucket, fmt.Sprintf("%s/%020d", id, row.Number), row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

func TestRetentionByAge(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	t0 := clock.Now()
	seedLegacy(h, "legacy-old", Progress{Status: StateCompleted, StartedAt: t0.Add(-time.Hour), UpdatedAt: t0}, []Row{{Number: 1, Key: "a", Status: StatusMissingTarget}})
	seedLegacy(h, "legacy-new", Progress{Status: StateCompleted, StartedAt: t0.Add(-time.Hour), UpdatedAt: t0.AddDate(0, 0, 20)}, nil)
	m := h.open(storesFactory(sourceOf(3000), newFakeStore()), func(o *Options) { o.Now = clock.Now })
	startAndWait(t, m, requestWithID("old"))
	clock.advance(10 * 24 * time.Hour)
	startAndWait(t, m, requestWithID("recent"))
	clock.advance(21 * 24 * time.Hour)
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "recent,legacy-new" {
		t.Fatalf("age retention kept %v", ids)
	}
	assertGone(t, h, m, "old")
	assertGone(t, h, m, "legacy-old")
	if rows := allRows(t, m, RowsQuery{ID: "recent", Limit: MaxPageRows}); len(rows) != 3000 {
		t.Fatalf("kept comparison lost rows: %d", len(rows))
	}
}

func TestRetentionByBudgetNeverRemovesTheRunningComparison(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	source := sourceOf(40)
	m := h.open(storesFactory(source, newFakeStore()), func(o *Options) { o.Now = clock.Now })
	a := startAndWait(t, m, requestWithID("a"))
	size := a.StoredBytes
	m.Close()
	// Budget for two and a half comparisons.
	m = h.open(storesFactory(source, newFakeStore()), func(o *Options) { o.Now, o.BudgetBytes = clock.Now, size*5/2 })
	clock.advance(time.Second)
	startAndWait(t, m, requestWithID("b"))
	clock.advance(time.Second)
	startAndWait(t, m, requestWithID("c"))
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "c,b" {
		t.Fatalf("budget retention kept %v", ids)
	}
	assertGone(t, h, m, "a")
	// The running comparison started first, so it is the oldest; retention
	// skips it and removes the oldest finished one instead.
	clock.advance(-time.Hour)
	blocked := sourceOf(41)
	blocked.listHook = blockAfter("src/0039")
	m.flushEvery = time.Millisecond
	if _, err := m.Start(requestWithID("running"), storesFactory(&pagedStore{fakeStore: blocked, limit: 10}, newFakeStore())); err != nil {
		t.Fatal(err)
	}
	// How many rows an engine emits before its LIST blocks varies; any count
	// between half and all 40 rows leads to the same choice.
	waitFor(t, "running comparison committing its rows", func() bool { return h.record("running").Rows >= 30 })
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "c,running" {
		t.Fatalf("budget retention with a running comparison kept %v", ids)
	}
	if p, err := m.Get("running", 0, 1); err != nil || p.Status != StateRunning || p.StoredBytes == 0 || p.StoredBytes > size {
		t.Fatalf("running comparison touched: %+v %v", p.Progress, err)
	}
	if _, err := m.Cancel("running"); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	if rows := allRows(t, m, RowsQuery{ID: "running", Limit: MaxPageRows}); int64(len(rows)) != h.record("running").Rows || len(rows) < 30 {
		t.Fatalf("running comparison lost rows: %d", len(rows))
	}
}

func TestRetentionByCount(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	seedLegacy(h, "legacy", Progress{Status: StateCompleted, StartedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now()}, nil)
	m := h.open(storesFactory(sourceOf(5), newFakeStore()), func(o *Options) { o.Now, o.MaxRetained = clock.Now, 2 })
	for _, id := range []string{"a", "b", "c"} {
		clock.advance(time.Second)
		startAndWait(t, m, requestWithID(id))
	}
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "c,b" {
		t.Fatalf("count retention kept %v", ids)
	}
	assertGone(t, h, m, "a")
	assertGone(t, h, m, "legacy")
}

func TestRetentionDeletionSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	factory := storesFactory(sourceOf(3000), newFakeStore())
	m := h.open(factory)
	for _, id := range []string{"marked", "removed-directory", "other"} {
		startAndWait(t, m, requestWithID(id))
	}
	m.Close()
	// Crashes after each deletion step: marked only, and marked with its
	// directory already removed. A legacy deletion left its tombstone.
	for _, id := range []string{"marked", "removed-directory"} {
		rec := h.record(id)
		rec.Deleting = true
		h.put(runsBucket, id, rec)
	}
	if err := os.RemoveAll(filepath.Join(h.dir, resultDirectory("removed-directory"))); err != nil {
		t.Fatal(err)
	}
	seedLegacy(h, "legacy", Progress{Status: StateCompleted}, []Row{{Number: 1, Key: "a", Status: StatusError}, {Number: 2, Key: "b", Status: StatusError}})
	if err := h.db.Update(func(tx *state.Tx) error {
		if err := tx.Delete(legacyBucket, "legacy"); err != nil {
			return err
		}
		return tx.Put(runsBucket, "legacy", runRecord{Progress: Progress{Request: requestWithID("legacy")}, Deleting: true, Legacy: true})
	}); err != nil {
		t.Fatal(err)
	}
	m = h.open(factory)
	m.retain()
	for _, id := range []string{"marked", "removed-directory", "legacy"} {
		assertGone(t, h, m, id)
	}
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "other" {
		t.Fatalf("listed %v", ids)
	}
	// A comparison being deleted is hidden and its ID is not reused until the
	// deletion finished.
	rec := h.record("other")
	rec.Deleting = true
	h.put(runsBucket, "other", rec)
	if _, err := m.Start(requestWithID("other")); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("started over a deletion: %v", err)
	}
	if err := m.Delete("other"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, h, m, "other")
	if p := startAndWait(t, m, requestWithID("other")); p.Status != StateCompleted || p.Rows != 3000 {
		t.Fatalf("ID not reusable after deletion: %+v", p)
	}
}

func TestRetentionRemovesOrphanResultDirectories(t *testing.T) {
	h := newHarness(t)
	factory := storesFactory(sourceOf(10), newFakeStore())
	m := h.open(factory)
	startAndWait(t, m, request())
	m.Close()
	orphan := filepath.Join(h.dir, "0123456789abcdef0123456789abcdef")
	keep := filepath.Join(h.dir, "keep-me")
	file := filepath.Join(h.dir, "fedcba9876543210fedcba9876543210")
	for _, dir := range []string{orphan, keep} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, resultsFile), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m = h.open(factory)
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan kept: %v", err)
	}
	for _, path := range []string{keep, file, h.resultsPath("test")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("removed %s: %v", path, err)
		}
	}
	if rows := allRows(t, m, RowsQuery{ID: "test", Limit: 100}); len(rows) != 10 {
		t.Fatalf("rows %d", len(rows))
	}
}

func TestFreeDiskBytesProbe(t *testing.T) {
	free, err := freeDiskBytes(t.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("free space probe: %d %v", free, err)
	}
	if _, err := freeDiskBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("probe of a missing directory succeeded")
	}
}

func smallSource(prefix string, n int) *fakeStore {
	var objects []transferstore.Object
	for i := 0; i < n; i++ {
		objects = append(objects, object(fmt.Sprintf("src/%s%04d", prefix, i), 1))
	}
	return newFakeStore(objects...)
}

// Budget retention removes the comparison that ended first. A comparison that
// was paused, resumed and has just completed keeps its early start time, but it
// ended last, so it must survive.
func TestRetentionBudgetRemovesTheEarliestEndedComparison(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	// Measure the stored sizes with a probe Manager.
	m := h.open(storesFactory(sourceOf(400), newFakeStore()), func(o *Options) { o.Now = clock.Now })
	full := startAndWait(t, m, requestWithID("probe-full")).StoredBytes
	small := startAndWait(t, m, requestWithID("probe-small"), storesFactory(smallSource("b", 20), newFakeStore())).StoredBytes
	for _, id := range []string{"probe-full", "probe-small"} {
		if err := m.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	m.Close()
	budget := full + 2*small - 1
	m = h.open(storesFactory(sourceOf(400), newFakeStore()), func(o *Options) { o.Now, o.BudgetBytes = clock.Now, budget })
	m.flushEvery = time.Millisecond

	// A starts first and is paused about halfway.
	blocked := sourceOf(400)
	blocked.listHook = blockAfter("src/0199")
	if _, err := m.Start(requestWithID("A"), storesFactory(&pagedStore{fakeStore: blocked, limit: 10}, newFakeStore())); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "A commits rows", func() bool { return h.record("A").Rows >= 100 })
	if _, err := m.Cancel("A"); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	if p, _ := m.Get("A", 0, 1); p.Status != StateCanceled || !p.Resumable {
		t.Fatalf("A not paused: %+v", p.Progress)
	}
	clock.advance(time.Second)
	startAndWait(t, m, requestWithID("B"), storesFactory(smallSource("b", 20), newFakeStore()))
	clock.advance(time.Second)
	startAndWait(t, m, requestWithID("C"), storesFactory(smallSource("c", 20), newFakeStore()))
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "C,B,A" {
		t.Fatalf("before resume kept %v", ids)
	}
	clock.advance(time.Second)
	if _, err := m.Resume("A", storesFactory(sourceOf(400), newFakeStore())); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	m.retain()
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "C,A" {
		t.Fatalf("budget %d (A %d, B and C %d each) kept %v, want C,A", budget, full, small, ids)
	}
}
