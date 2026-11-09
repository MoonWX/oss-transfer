package comparison

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// sameResults compares the outcome of a resumed comparison with an
// uninterrupted one over the same data.
func sameResults(t *testing.T, m *Manager, resumed, reference string) {
	t.Helper()
	a, err := m.Get(resumed, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Get(reference, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StateCompleted || a.Compared != b.Compared || a.Matched != b.Matched || a.Different != b.Different || a.Missing != b.Missing || a.Extra != b.Extra || a.Errors != b.Errors || a.Rows != b.Rows || a.Checkpoint != b.Checkpoint || a.StoredBytes != b.StoredBytes {
		t.Fatalf("resumed totals differ:\n%+v\n%+v", a.Progress, b.Progress)
	}
	x := allRows(t, m, RowsQuery{ID: resumed, Limit: MaxPageRows})
	y := allRows(t, m, RowsQuery{ID: reference, Limit: MaxPageRows})
	if len(x) != len(y) {
		t.Fatalf("resumed rows %d, uninterrupted %d", len(x), len(y))
	}
	for i := range x {
		if x[i].Number != int64(i+1) || x[i].Key != y[i].Key || x[i].Status != y[i].Status || x[i].SourceSize != y[i].SourceSize || x[i].DestinationSize != y[i].DestinationSize {
			t.Fatalf("row %d differs: %+v %+v", i+1, x[i], y[i])
		}
	}
}

// interrupt runs "test" until LIST blocks halfway, then shuts the Manager
// down. It returns the Manager for the reference run's database.
func interrupt(t *testing.T, h *harness) {
	t.Helper()
	source, target, _ := mixedStores(6000, 50)
	m := h.open(storesFactory(source, target))
	startAndWait(t, m, requestWithID("reference"))
	blocked, target2, _ := mixedStores(6000, 50)
	blocked.listHook = blockAfter("src/0003000")
	if _, err := m.Start(request(), storesFactory(blocked, target2)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a committed checkpoint", func() bool { return h.record("test").Compared >= 2000 })
	m.Close()
	rec := h.record("test")
	if rec.Status != StateInterrupted || !rec.AutoResume || !rec.Resumable || rec.Compared < 2000 || rec.Compared >= 6050 || rec.EndedAt.IsZero() {
		t.Fatalf("shutdown did not interrupt: %+v", rec)
	}
}

func TestResumeAfterRestartMatchesAnUninterruptedRun(t *testing.T) {
	for _, mode := range []string{"shutdown", "crash"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			interrupt(t, h)
			if mode == "crash" {
				// Killed instead: still RUNNING, with an uncommitted tail.
				rec := h.record("test")
				rec.Status, rec.AutoResume, rec.Resumable, rec.EndedAt, rec.ExpiresAt = StateRunning, false, false, time.Time{}, time.Time{}
				h.put(runsBucket, "test", rec)
				file, err := os.OpenFile(h.resultsPath("test"), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString(`{"number":1,"key":"uncommitted","status":"ERROR"}` + "\n"); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			source, target, _ := mixedStores(6000, 50)
			m := h.open(storesFactory(source, target))
			waitWorkers(t, m)
			sameResults(t, m, "test", "reference")
			rec := h.record("test")
			if rec.Resumes != 1 || rec.UncleanResumes != 0 || rec.AutoResume || rec.Resumable {
				t.Fatalf("resume bookkeeping: %+v", rec)
			}
		})
	}
}

func TestResumeRevisionMismatchWaitsForManualResume(t *testing.T) {
	h := newHarness(t)
	interrupt(t, h)
	source, target, _ := mixedStores(6000, 50)
	factory := storesFactory(source, target)
	var asked []Request
	m := h.open(factory, func(o *Options) {
		o.ResumeFactory = func(r Request) (execution.StoreFactory, error) {
			asked = append(asked, r)
			return nil, errors.New("storage connections changed; resume manually")
		}
	})
	waitWorkers(t, m)
	p, err := m.Get("test", 0, 1)
	if err != nil || p.Status != StateInterrupted || !p.Resumable || p.ErrorDetail != "storage connections changed; resume manually" || h.record("test").AutoResume {
		t.Fatalf("revision mismatch: %+v %v", p.Progress, err)
	}
	if len(asked) != 1 || asked[0] != request() {
		t.Fatalf("resume factory asked for %+v", asked)
	}
	if !h.log.contains("comparison test: not resumed automatically: storage connections changed") {
		t.Fatalf("not logged: %q", h.log.lines)
	}
	if p, err := m.Resume("test", factory); err != nil || p.Status != StateRunning || p.ErrorDetail != "" || p.Resumes != 1 {
		t.Fatalf("manual resume: %+v %v", p, err)
	}
	waitWorkers(t, m)
	sameResults(t, m, "test", "reference")
}

func TestResumeOnlyTheMostRecentAutomatically(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	factory := storesFactory(sourceOf(20), newFakeStore())
	m := h.open(factory, func(o *Options) { o.Now = clock.Now })
	startAndWait(t, m, requestWithID("older"))
	clock.advance(time.Minute)
	startAndWait(t, m, requestWithID("newer"))
	m.Close()
	for _, id := range []string{"older", "newer"} {
		rec := h.record(id)
		rec.Status = StateRunning
		h.put(runsBucket, id, rec)
	}
	m = h.open(factory, func(o *Options) { o.Now = clock.Now })
	waitWorkers(t, m)
	newer, older := h.record("newer"), h.record("older")
	if newer.Status != StateCompleted || newer.Resumes != 1 || newer.Compared != 20 || newer.Rows != 20 {
		t.Fatalf("newest not resumed: %+v", newer)
	}
	if older.Status != StateInterrupted || older.Resumes != 0 || !older.Resumable || older.AutoResume || older.EndedAt.IsZero() {
		t.Fatalf("older comparison: %+v", older)
	}
}

func TestResumeStopsAfterRepeatedUncleanRestarts(t *testing.T) {
	h := newHarness(t)
	factory := storesFactory(sourceOf(20), newFakeStore())
	m := h.open(factory)
	startAndWait(t, m, request())
	m.Close()
	rec := h.record("test")
	rec.Status, rec.UncleanResumes = StateRunning, maxUncleanResumes
	h.put(runsBucket, "test", rec)
	m = h.open(factory)
	waitWorkers(t, m)
	rec = h.record("test")
	if rec.Status != StateInterrupted || rec.Resumes != 0 || !rec.Resumable || rec.UncleanResumes != 0 || !strings.Contains(rec.ErrorDetail, "restarts without a clean stop") {
		t.Fatalf("crash loop not stopped: %+v", rec)
	}
	if _, err := m.Resume("test", nil); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	if rec = h.record("test"); rec.Status != StateCompleted || rec.Compared != 20 || rec.Rows != 20 || rec.Resumes != 1 {
		t.Fatalf("manual resume: %+v", rec)
	}
}

func TestResumeProgressClearsTheRestartCount(t *testing.T) {
	h := newHarness(t)
	interrupt(t, h)
	rec := h.record("test")
	rec.Status, rec.AutoResume, rec.UncleanResumes = StateRunning, false, maxUncleanResumes-1
	h.put(runsBucket, "test", rec)
	source, target, _ := mixedStores(6000, 50)
	source.listHook = blockAfter("src/0005000")
	m := h.open(storesFactory(source, target))
	waitFor(t, "progress after the resume", func() bool {
		r := h.record("test")
		return r.Status == StateRunning && r.Compared > rec.Compared && r.UncleanResumes == 0
	})
	if _, err := m.Cancel("test"); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	if r := h.record("test"); r.Status != StateCanceled || r.Resumes != 1 {
		t.Fatalf("resumed run: %+v", r)
	}
}

func TestResumeErrors(t *testing.T) {
	h := newHarness(t)
	factory := storesFactory(sourceOf(5), newFakeStore())
	m := h.open(factory)
	startAndWait(t, m, requestWithID("completed"))
	m.commitHook = func(rec *runRecord) error {
		if rec.Rows > 0 {
			return errors.New("disk failed")
		}
		return nil
	}
	if p := startAndWait(t, m, requestWithID("unsaved")); p.Status != StateFailed || p.Error != errorSave || p.Resumable {
		t.Fatalf("durable failure: %+v", p)
	}
	m.commitHook = nil
	blocked := sourceOf(5)
	blocked.listHook = blockAfter("")
	if _, err := m.Start(requestWithID("canceled"), storesFactory(blocked, newFakeStore())); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel("canceled"); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	for id, want := range map[string]error{"missing": ErrNotFound, "": ErrNotFound, "completed": ErrNotResumable, "unsaved": ErrNotResumable} {
		if _, err := m.Resume(id, nil); !errors.Is(err, want) {
			t.Fatalf("resume %q: %v, want %v", id, err, want)
		}
	}
	busy := sourceOf(5)
	busy.listHook = blockAfter("")
	if _, err := m.Start(requestWithID("busy"), storesFactory(busy, newFakeStore())); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume("canceled", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("resumed beside a running comparison: %v", err)
	}
	if p, err := m.Resume("busy", nil); err != nil || p.Status != StateRunning {
		t.Fatalf("resuming the running comparison is not idempotent: %+v %v", p, err)
	}
	if err := m.Delete("busy"); !errors.Is(err, ErrRunning) {
		t.Fatalf("deleted a running comparison: %v", err)
	}
	if _, err := m.Cancel("busy"); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	if p, err := m.Resume("canceled", nil); err != nil || p.Status != StateRunning || p.Resumes != 1 {
		t.Fatalf("resume of a canceled comparison: %+v %v", p, err)
	}
	waitWorkers(t, m)
	if rec := h.record("canceled"); rec.Status != StateCompleted || rec.Compared != 5 || rec.Rows != 5 {
		t.Fatalf("canceled comparison did not finish: %+v", rec)
	}
}

func TestUnavailableResultsAreReportedNotResumed(t *testing.T) {
	h := newHarness(t)
	factory := storesFactory(sourceOf(3000), newFakeStore())
	m := h.open(factory)
	for _, id := range []string{"missing", "short", "crashed"} {
		startAndWait(t, m, requestWithID(id))
	}
	m.Close()
	if err := os.Remove(h.resultsPath("missing")); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(h.resultsPath("short"), h.record("short").Committed/2); err != nil {
		t.Fatal(err)
	}
	rec := h.record("crashed")
	rec.Status = StateRunning
	h.put(runsBucket, "crashed", rec)
	if err := os.Remove(h.resultsPath("crashed")); err != nil {
		t.Fatal(err)
	}
	// Lost results no longer use the byte budget: a budget for one
	// comparison removes none of them.
	size := h.record("missing").StoredBytes
	m = h.open(factory, func(o *Options) { o.BudgetBytes = size })
	waitWorkers(t, m)
	m.retain()
	if ids := listedIDs(t, m); len(ids) != 3 {
		t.Fatalf("retention removed comparisons without results: %v", ids)
	}
	for id, detail := range map[string]string{"missing": "result file missing", "short": "result file is shorter than its committed length", "crashed": "result file missing"} {
		page, err := m.Rows(RowsQuery{ID: id, Limit: 10})
		p := page.Progress
		if err != nil || len(page.Items) != 0 || page.NextAfter != 0 || p.Error != errorUnavailable || p.ErrorDetail != detail || p.Resumable || p.Rows != 3000 || p.Compared != 3000 {
			t.Fatalf("%s: %+v %v", id, p, err)
		}
		if _, err := m.Resume(id, nil); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("%s resumed: %v", id, err)
		}
	}
	if p, _ := m.Get("crashed", 0, 1); p.Status != StateInterrupted || p.Resumes != 0 {
		t.Fatalf("crashed comparison without results: %+v", p.Progress)
	}
	if p, _ := m.Get("missing", 0, 1); p.Status != StateCompleted {
		t.Fatalf("completed comparison changed state: %+v", p.Progress)
	}
	if err := m.Delete("missing"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, h, m, "missing")
}
