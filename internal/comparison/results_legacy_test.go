package comparison

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLegacyResultsReadableFilterableDeletable(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	t0 := clock.Now()
	statuses := []string{StatusMissingTarget, StatusDifferent, StatusError}
	var rows []Row
	for i := 1; i <= 300; i++ {
		rows = append(rows, Row{Number: int64(i), Key: string(rune('a'+i%26)) + strings.Repeat("k", i), Status: statuses[i%3]})
	}
	seedLegacy(h, "old", Progress{Status: StateCompleted, Compared: 12000, Missing: 11000, Different: 1000, RowsTruncated: true, StartedAt: t0.Add(-2 * time.Hour), UpdatedAt: t0.Add(-time.Hour)}, rows)
	seedLegacy(h, "stale", Progress{Status: StateRunning, StartedAt: t0.Add(-3 * time.Hour), UpdatedAt: t0.Add(-3 * time.Hour)}, nil)
	h.put(runtimeBucket, "active", "stale")
	m := h.open(storesFactory(newFakeStore(), newFakeStore()), func(o *Options) { o.Now = clock.Now })
	if h.exists(runtimeBucket, "active") {
		t.Fatal("stale active marker kept")
	}
	page, err := m.Get("old", 0, MaxLegacyPageRows)
	if err != nil || len(page.Items) != 200 || page.NextAfter != 200 || page.Items[0].Number != 1 {
		t.Fatalf("legacy page: %d %d %v", len(page.Items), page.NextAfter, err)
	}
	p := page.Progress
	if p.ResultLimit != LegacyStoredDifferences || p.TruncatedReason != TruncatedLegacyLimit || p.ResultsProtocol != 0 || p.Resumable || p.Compared != 12000 || p.Rows != 300 || !p.ExpiresAt.Equal(p.UpdatedAt.Add(30*24*time.Hour)) {
		t.Fatalf("legacy progress: %+v", p)
	}
	if page, err = m.Get("old", 200, MaxLegacyPageRows); err != nil || len(page.Items) != 100 || page.NextAfter != 0 {
		t.Fatalf("legacy last page: %d %d %v", len(page.Items), page.NextAfter, err)
	}
	different := allRows(t, m, RowsQuery{ID: "old", Limit: 7, Statuses: []string{StatusDifferent}})
	if len(different) != 100 {
		t.Fatalf("legacy filter returned %d rows", len(different))
	}
	for _, row := range different {
		if row.Status != StatusDifferent || rows[row.Number-1].Key != row.Key {
			t.Fatalf("legacy filter returned %+v", row)
		}
	}
	page, err = m.Rows(RowsQuery{ID: "old", Limit: MaxPageRows, MaxBytes: 2000})
	if err != nil || len(page.Items) == 0 || len(page.Items) == 300 || encodedSize(t, page.Items) > 2000 || page.NextAfter != page.Items[len(page.Items)-1].Number {
		t.Fatalf("legacy byte cap: %d rows %v", len(page.Items), err)
	}
	if all := allRows(t, m, RowsQuery{ID: "old", Limit: MaxPageRows, MaxBytes: 2000}); len(all) != 300 {
		t.Fatalf("legacy paging lost rows: %d", len(all))
	}
	if _, err := m.Rows(RowsQuery{ID: "old", Limit: 1, Statuses: []string{"NOPE"}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("invalid legacy filter accepted")
	}
	stale, err := m.Get("stale", 0, 1)
	if err != nil || stale.Status != StateInterrupted || stale.Resumable {
		t.Fatalf("stale legacy run: %+v %v", stale.Progress, err)
	}
	if ids := listedIDs(t, m); strings.Join(ids, ",") != "old,stale" {
		t.Fatalf("listed %v", ids)
	}
	// The ID stays bound to its request.
	if p, err := m.Start(requestWithID("old")); err != nil || p.TruncatedReason != TruncatedLegacyLimit || p.Status != StateCompleted {
		t.Fatalf("legacy start not idempotent: %+v %v", p, err)
	}
	changed := requestWithID("old")
	changed.SourcePrefix = "other/"
	if _, err := m.Start(changed); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("legacy ID reused: %v", err)
	}
	if _, err := m.Resume("old", nil); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("legacy comparison resumed: %v", err)
	}
	if p, err := m.Cancel("old"); err != nil || p.Status != StateCompleted {
		t.Fatalf("cancel of a finished legacy comparison: %+v %v", p, err)
	}
	if err := m.Delete("old"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, h, m, "old")
	if err := m.Delete("stale"); err != nil {
		t.Fatal(err)
	}
	if ids := listedIDs(t, m); len(ids) != 0 {
		t.Fatalf("listed %v", ids)
	}
}
