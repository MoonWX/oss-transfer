package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// mixedStores builds n source keys. Every 7th has an equal target object
// (MATCH), every 11th a target of another size (DIFFERENT), the rest are
// missing; extra target-only keys follow the source keys.
func mixedStores(n, extra int) (*fakeStore, *fakeStore, map[string]string) {
	var source, target []transferstore.Object
	want := map[string]string{}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%07d", i)
		source = append(source, object("src/"+key, 1))
		switch {
		case i%7 == 0:
			target = append(target, object("backup/"+key, 1))
			want[key] = StatusMatch
		case i%11 == 0:
			target = append(target, object("backup/"+key, 2))
			want[key] = StatusDifferent
		default:
			want[key] = StatusMissingTarget
		}
	}
	for i := 0; i < extra; i++ {
		key := fmt.Sprintf("zz-extra-%04d", i)
		target = append(target, object("backup/"+key, 1))
		want[key] = StatusExtraTarget
	}
	return newFakeStore(source...), newFakeStore(target...), want
}

func checkRows(t *testing.T, rows []Row, want map[string]string) {
	t.Helper()
	differences := 0
	for _, status := range want {
		if status != StatusMatch {
			differences++
		}
	}
	if len(rows) != differences {
		t.Fatalf("stored %d rows, want %d", len(rows), differences)
	}
	for i, row := range rows {
		if row.Number != int64(i+1) {
			t.Fatalf("row %d numbered %d", i+1, row.Number)
		}
		if i > 0 && row.Key <= rows[i-1].Key {
			t.Fatalf("rows out of key order at %d", row.Number)
		}
		if want[row.Key] != row.Status || row.Status == StatusMatch {
			t.Fatalf("wrong row %+v (want %s)", row, want[row.Key])
		}
	}
}

func TestResultsStoreEveryDifferenceAcrossManyBlocks(t *testing.T) {
	h := newHarness(t)
	source, target, want := mixedStores(30_000, 1_500)
	m := h.open(storesFactory(source, target))
	p := startAndWait(t, m, request())
	var matched, different, missing, extra int64
	for _, status := range want {
		switch status {
		case StatusMatch:
			matched++
		case StatusDifferent:
			different++
		case StatusMissingTarget:
			missing++
		case StatusExtraTarget:
			extra++
		}
	}
	if p.Status != StateCompleted || p.Compared != int64(len(want)) || p.Matched != matched || p.Different != different || p.Missing != missing || p.Extra != extra || p.Rows != p.Compared-p.Matched || p.RowsTruncated || p.Checkpoint != "zz-extra-1499" {
		t.Fatalf("wrong totals: %+v", p)
	}
	if p.Rows <= 25_000 || p.StoredBytes != h.fileSize("test") || p.StoredBytes != h.record("test").Committed {
		t.Fatalf("stored bytes do not match the file: %+v", p)
	}
	if blocks := h.count(blocksBucket, "test/"); blocks != int((p.Rows+blockRows-1)/blockRows) {
		t.Fatalf("%d index blocks for %d rows", blocks, p.Rows)
	}
	checkRows(t, allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows}), want)
	checkRows(t, allRows(t, m, RowsQuery{ID: "test", Limit: 37, MaxBytes: 2048}), want)
	// comparison_get pages the same rows for Masters without comparison_rows.
	var legacy []Row
	for after := int64(0); ; {
		page, err := m.Get("test", after, MaxLegacyPageRows)
		if err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, page.Items...)
		if page.NextAfter == 0 {
			break
		}
		after = page.NextAfter
	}
	checkRows(t, legacy, want)
	for _, status := range []string{StatusDifferent, StatusExtraTarget} {
		filtered := allRows(t, m, RowsQuery{ID: "test", Limit: 100, Statuses: []string{status}})
		subset := map[string]string{}
		for key, s := range want {
			if s == status {
				subset[key] = s
			}
		}
		if len(filtered) != len(subset) {
			t.Fatalf("%s filter returned %d of %d rows", status, len(filtered), len(subset))
		}
		for _, row := range filtered {
			if subset[row.Key] != row.Status {
				t.Fatalf("filter returned %+v", row)
			}
		}
	}
}

func TestResultsStatusFilterSkipsBlocksWithinScanBudget(t *testing.T) {
	h := newHarness(t)
	// 65 blocks of missing targets, then three extra targets; one block in
	// the middle has the only DIFFERENT row.
	missing := (scanBlocks+1)*blockRows - 1
	var source, target []transferstore.Object
	for i := 0; i < missing+1; i++ {
		key := fmt.Sprintf("%06d", i)
		source = append(source, object("src/"+key, 1))
		if i == 40*blockRows+7 {
			target = append(target, object("backup/"+key, 2))
		}
	}
	for i := 0; i < 3; i++ {
		target = append(target, object(fmt.Sprintf("backup/zz%d", i), 1))
	}
	m := h.open(storesFactory(newFakeStore(source...), newFakeStore(target...)))
	p := startAndWait(t, m, request())
	if p.Rows != int64(missing+4) || p.Extra != 3 || p.Different != 1 {
		t.Fatalf("wrong totals: %+v", p)
	}
	// Prove that blocks without a requested status are never read: destroy
	// the rows of block 10 (missing targets only).
	rec := h.record("test")
	var block resultBlock
	if err := h.db.View(func(tx *state.Tx) error {
		_, err := tx.Get(blocksBucket, blockKey("test", 10), &block)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(h.resultsPath("test"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte(strings.Repeat("x", int(block.Length))), block.Offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rows(RowsQuery{ID: "test", After: 10 * blockRows, Limit: 10}); !errors.Is(err, errResultsDamaged) {
		t.Fatalf("damaged block was not detected: %v", err)
	}
	page, err := m.Rows(RowsQuery{ID: "test", Limit: 10, Statuses: []string{StatusExtraTarget}})
	if err != nil || len(page.Items) != 0 || page.NextAfter != scanBlocks*blockRows {
		t.Fatalf("scan budget not applied: %d items, next %d, %v", len(page.Items), page.NextAfter, err)
	}
	page, err = m.Rows(RowsQuery{ID: "test", After: page.NextAfter, Limit: 2, Statuses: []string{StatusExtraTarget}})
	if err != nil || len(page.Items) != 2 || page.Items[0].Key != "zz0" || page.Items[0].Number != rec.Rows-2 || page.NextAfter != rec.Rows-1 {
		t.Fatalf("extra targets not found after the budget: %+v %v", page.Items, err)
	}
	page, err = m.Rows(RowsQuery{ID: "test", After: page.NextAfter, Limit: 2, Statuses: []string{StatusExtraTarget}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != "zz2" || page.NextAfter != 0 {
		t.Fatalf("last page wrong: %+v %v", page, err)
	}
	page, err = m.Rows(RowsQuery{ID: "test", Limit: 5, Statuses: []string{StatusDifferent, StatusExtraTarget}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != StatusDifferent || page.Items[0].Number != 40*blockRows+8 {
		t.Fatalf("multi-status filter wrong: %+v %v", page.Items, err)
	}
}

func TestResultsPageBytesWithLongKeys(t *testing.T) {
	h := newHarness(t)
	// 1,024-byte source keys. JSON escapes '<' as a six-byte \u sequence, so
	// each key costs about 6 KB per field and a row about 18 KB.
	var source []transferstore.Object
	for i := 0; i < 300; i++ {
		source = append(source, object(fmt.Sprintf("src/%04d%s", i, strings.Repeat("<", 1016)), 1))
	}
	if len(source[0].Key) != 1024 {
		t.Fatal("key length", len(source[0].Key))
	}
	m := h.open(storesFactory(newFakeStore(source...), newFakeStore()))
	if p := startAndWait(t, m, request()); p.Rows != 300 {
		t.Fatalf("rows not stored: %+v", p)
	}
	total, after := 0, int64(0)
	for {
		page, err := m.Rows(RowsQuery{ID: "test", After: after, Limit: MaxPageRows})
		if err != nil {
			t.Fatal(err)
		}
		size := encodedSize(t, page.Items)
		if len(page.Items) == 0 || size > MaxPageBytes || len(page.Items) == 300 {
			t.Fatalf("page of %d rows and %d bytes", len(page.Items), size)
		}
		encoded, _ := json.Marshal(page)
		if len(encoded) > 8<<20 {
			t.Fatalf("page exceeds the Agent payload limit: %d", len(encoded))
		}
		total += len(page.Items)
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter != page.Items[len(page.Items)-1].Number {
			t.Fatalf("cursor skipped rows: %d", page.NextAfter)
		}
		after = page.NextAfter
	}
	if total != 300 {
		t.Fatalf("paged %d rows", total)
	}
	// A single row larger than MaxBytes is still returned, one per page.
	for after := int64(0); after < 3; after++ {
		page, err := m.Rows(RowsQuery{ID: "test", After: after, Limit: MaxPageRows, MaxBytes: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].Number != after+1 || page.NextAfter != after+1 {
			t.Fatalf("oversized row page: %d %d %v", len(page.Items), page.NextAfter, err)
		}
	}
}

func TestResultsCrashBetweenWriteAndCommit(t *testing.T) {
	h := newHarness(t)
	var source []transferstore.Object
	for i := 0; i < 3000; i++ {
		source = append(source, object(fmt.Sprintf("src/%05d", i), 1))
	}
	factory := storesFactory(newFakeStore(source...), newFakeStore())
	m := h.open(factory)
	// After the first commit with rows, every commit that adds rows fails
	// after they were written and synced: the state a crash before the commit
	// leaves behind.
	var committed atomic.Int64
	m.commitHook = func(rec *runRecord) error {
		if c := committed.Load(); c > 0 && rec.Rows > c {
			return errors.New("injected commit failure")
		}
		committed.Store(rec.Rows)
		return nil
	}
	p := startAndWait(t, m, request())
	if p.Status != StateFailed || p.Error != errorSave || p.ErrorDetail != "commit comparison results: injected commit failure" || p.Resumable {
		t.Fatalf("commit failure not reported: %+v", p)
	}
	if p.Rows == 0 || p.Rows != committed.Load() || p.Compared != p.Rows || p.Checkpoint != fmt.Sprintf("%05d", p.Rows-1) || p.StoredBytes >= h.fileSize("test") {
		t.Fatalf("uncommitted state exposed: %+v (file %d)", p, h.fileSize("test"))
	}
	if rows := allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows}); int64(len(rows)) != p.Rows {
		t.Fatalf("read %d rows past the commit", len(rows))
	}
	if !h.log.contains("comparison test failed: commit comparison results: injected commit failure") {
		t.Fatalf("failure not logged: %q", h.log.lines)
	}
	m.Close()
	// A process killed before recording the failure leaves RUNNING plus an
	// uncommitted tail; add a torn partial row as well.
	rec := h.record("test")
	rec.Status, rec.Error, rec.ErrorDetail = StateRunning, "", ""
	h.put(runsBucket, "test", rec)
	file, err := os.OpenFile(h.resultsPath("test"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"number":99999,"key":"torn`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	m = h.open(factory)
	waitWorkers(t, m)
	page, err := m.Get("test", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Status != StateCompleted || page.Compared != 3000 || page.Missing != 3000 || page.Rows != 3000 || page.Resumes != 1 || page.StoredBytes != h.fileSize("test") {
		t.Fatalf("resume after the crash did not finish exactly: %+v", page.Progress)
	}
	rows := allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows})
	for i, row := range rows {
		if row.Number != int64(i+1) || row.Key != fmt.Sprintf("%05d", i) {
			t.Fatalf("gap or duplicate at %d: %+v", i+1, row)
		}
	}
	if len(rows) != 3000 || h.record("test").UncleanResumes != 0 {
		t.Fatalf("resumed rows %d", len(rows))
	}
}

func TestResultsStorageBudgetKeepsCountsExact(t *testing.T) {
	h := newHarness(t)
	var source []transferstore.Object
	for i := 0; i < 2000; i++ {
		source = append(source, object(fmt.Sprintf("src/%05d", i), 1))
	}
	m := h.open(storesFactory(newFakeStore(source...), newFakeStore()), func(o *Options) { o.BudgetBytes = 64 << 10 })
	p := startAndWait(t, m, request())
	if p.Status != StateCompleted || !p.RowsTruncated || p.TruncatedReason != TruncatedStorageBudget || p.Compared != 2000 || p.Missing != 2000 {
		t.Fatalf("budget truncation wrong: %+v", p)
	}
	if p.Rows == 0 || p.Rows >= 2000 || p.StoredBytes > 64<<10 || p.LastStoredKey != fmt.Sprintf("%05d", p.Rows-1) {
		t.Fatalf("stored prefix wrong: %+v", p)
	}
	rows := allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows})
	if int64(len(rows)) != p.Rows || rows[len(rows)-1].Key != p.LastStoredKey {
		t.Fatalf("stored rows %d do not end at %q", len(rows), p.LastStoredKey)
	}
}

func TestResultsLowDiskSpaceKeepsCountsExact(t *testing.T) {
	h := newHarness(t)
	var source []transferstore.Object
	for i := 0; i < 5000; i++ {
		source = append(source, object(fmt.Sprintf("src/%05d", i), 1))
	}
	// A disk of 1 MiB + 150 KiB: free space shrinks as the result file grows.
	const capacity, minimum = 1<<20 + 150<<10, 1 << 20
	var probes atomic.Int64
	m := h.open(storesFactory(newFakeStore(source...), newFakeStore()), func(o *Options) {
		o.MinimumFreeBytes = minimum
		o.FreeBytes = func(directory string) (uint64, error) {
			probes.Add(1)
			info, err := os.Stat(h.resultsPath("test"))
			if err != nil {
				return 0, err
			}
			return uint64(capacity - info.Size()), nil
		}
	})
	p := startAndWait(t, m, request())
	if p.Status != StateCompleted || !p.RowsTruncated || p.TruncatedReason != TruncatedLowDiskSpace || p.Compared != 5000 || p.Missing != 5000 {
		t.Fatalf("low-disk truncation wrong: %+v", p)
	}
	if p.Rows == 0 || p.StoredBytes > capacity-minimum || p.LastStoredKey != fmt.Sprintf("%05d", p.Rows-1) || p.StoredBytes != h.fileSize("test") {
		t.Fatalf("stored prefix wrong: %+v", p)
	}
	if probes.Load() == 0 {
		t.Fatal("free space was not checked")
	}
	if rows := allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows}); int64(len(rows)) != p.Rows || rows[len(rows)-1].Key != p.LastStoredKey {
		t.Fatalf("stored rows %d", len(rows))
	}
}

func TestResultsCountEveryStatus(t *testing.T) {
	var p Progress
	for _, status := range []string{StatusMatch, StatusDifferent, StatusETagMismatch, StatusMissingTarget, StatusExtraTarget, StatusChanged, StatusError, StatusBothMissing, "SOMETHING_NEW"} {
		countRow(&p, status)
	}
	if p.Compared != 9 || p.Matched != 1 || p.Different != 1 || p.ETagMismatch != 1 || p.Missing != 1 || p.Extra != 1 || p.Changed != 1 || p.BothMissing != 1 || p.Errors != 2 {
		t.Fatalf("wrong counts: %+v", p)
	}
}

func TestResultsBothMissingIsNotAnError(t *testing.T) {
	h := newHarness(t)
	m := h.open(storesFactory(newFakeStore(), newFakeStore()))
	r := request()
	r.Scope, r.SourcePrefix, r.DestinationPrefix = "object", "src/gone", "backup/gone"
	p := startAndWait(t, m, r)
	if p.Status != StateCompleted || p.BothMissing != 1 || p.Errors != 0 || p.Rows != 1 || p.Resumable {
		t.Fatalf("BOTH_MISSING miscounted: %+v", p)
	}
	page, err := m.Rows(RowsQuery{ID: "test", Limit: 10, Statuses: []string{StatusBothMissing}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != StatusBothMissing {
		t.Fatalf("BOTH_MISSING row not stored: %+v %v", page.Items, err)
	}
	if _, err := m.Resume("test", nil); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("object comparison resumed: %v", err)
	}
}

// closeGate holds a store's Close until released.
type closeGate struct {
	*fakeStore
	entered, release chan struct{}
}

func (s *closeGate) Close() error {
	close(s.entered)
	<-s.release
	return s.fakeStore.Close()
}

func TestResultsCancelingIsCommittedBeforeTheRunStops(t *testing.T) {
	h := newHarness(t)
	source := newFakeStore(object("src/a", 1), object("src/b", 1))
	gate := &closeGate{fakeStore: source, entered: make(chan struct{}), release: make(chan struct{})}
	m := h.open(storesFactory(gate, newFakeStore()))
	if _, err := m.Start(request()); err != nil {
		t.Fatal(err)
	}
	p, err := m.Cancel("test")
	if err != nil || p.Status != StateCanceling {
		t.Fatalf("cancel: %+v %v", p, err)
	}
	if rec := h.record("test"); rec.Status != StateCanceling {
		t.Fatalf("CANCELING not committed: %s", rec.Status)
	}
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Minute): // hang guard only
		t.Fatal("the canceled run did not close its stores")
	}
	if got, err := m.Get("test", 0, 1); err != nil || got.Status != StateCanceling {
		t.Fatalf("status flickered: %+v %v", got.Progress, err)
	}
	close(gate.release)
	waitWorkers(t, m)
	if got, _ := m.Get("test", 0, 1); got.Status != StateCanceled || !got.Resumable || got.ExpiresAt.Sub(got.EndedAt) != 30*24*time.Hour {
		t.Fatalf("not canceled: %+v", got.Progress)
	}
	m.Close()
	// A record a crash left CANCELING is canceled at the next start.
	rec := h.record("test")
	rec.Status = StateCanceling
	h.put(runsBucket, "test", rec)
	m = h.open(storesFactory(newFakeStore(), newFakeStore()))
	if got, _ := m.Get("test", 0, 1); got.Status != StateCanceled || !got.Resumable || got.EndedAt.IsZero() {
		t.Fatalf("CANCELING left after restart: %+v", got.Progress)
	}
}

// Readers see a consistent committed snapshot while rows are appended.
func TestResultsReadsWhileRunning(t *testing.T) {
	h := newHarness(t)
	var source []transferstore.Object
	for i := 0; i < 8_000; i++ {
		source = append(source, object(fmt.Sprintf("src/%05d", i), 1))
	}
	store := newFakeStore(source...)
	m := h.open(storesFactory(store, newFakeStore()))
	m.flushEvery = 50 * time.Millisecond // also commit partial batches
	if _, err := m.Start(request()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan string, 8)
	for reader := 0; reader < 3; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for {
				page, err := m.Rows(RowsQuery{ID: "test", After: int64(reader * 997), Limit: 300, Statuses: []string{StatusMissingTarget}})
				if err != nil {
					failures <- err.Error()
					return
				}
				for i, row := range page.Items {
					if row.Number != int64(reader*997+i+1) || row.Number > page.Rows || row.Key != fmt.Sprintf("%05d", row.Number-1) {
						failures <- fmt.Sprintf("inconsistent row %+v of %d", row, page.Rows)
						return
					}
				}
				if page.Status != StateRunning {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(reader)
	}
	wg.Wait()
	waitWorkers(t, m)
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	if rows := allRows(t, m, RowsQuery{ID: "test", Limit: MaxPageRows}); len(rows) != 8_000 {
		t.Fatalf("rows %d", len(rows))
	}
}

func TestResultsQueryValidation(t *testing.T) {
	h := newHarness(t)
	m := h.open(storesFactory(newFakeStore(object("src/a", 1)), newFakeStore()))
	startAndWait(t, m, request())
	for _, q := range []RowsQuery{
		{ID: "test", Limit: 0}, {ID: "test", Limit: MaxPageRows + 1}, {ID: "test", After: -1, Limit: 1},
		{ID: "test", Limit: 1, MaxBytes: -1}, {ID: "test", Limit: 1, MaxBytes: MaxPageBytes + 1},
		{ID: "test", Limit: 1, Statuses: []string{StatusMatch}}, {ID: "test", Limit: 1, Statuses: []string{"missing_target"}},
	} {
		if _, err := m.Rows(q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("accepted %+v: %v", q, err)
		}
	}
	for _, limit := range []int{0, MaxLegacyPageRows + 1} {
		if _, err := m.Get("test", 0, limit); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("Get accepted limit %d: %v", limit, err)
		}
	}
	if _, err := m.Get("test", -1, 1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("Get accepted a negative cursor")
	}
	for _, id := range []string{"", "other"} {
		if _, err := m.Rows(RowsQuery{ID: id, Limit: 1}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown %q: %v", id, err)
		}
		if _, err := m.Cancel(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cancel unknown %q: %v", id, err)
		}
		if err := m.Delete(id); err != nil {
			t.Fatalf("delete absent %q: %v", id, err)
		}
	}
	page, err := m.Rows(RowsQuery{ID: "test", After: 5, Limit: 1})
	if err != nil || len(page.Items) != 0 || page.NextAfter != 0 || page.Items == nil {
		t.Fatalf("cursor after the end: %+v %v", page, err)
	}
	if p, err := m.Start(request()); err != nil || p.Status != StateCompleted {
		t.Fatalf("same request is not idempotent: %+v %v", p, err)
	}
	other := request()
	other.Mode = "content"
	if _, err := m.Start(other); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("reused ID accepted: %v", err)
	}
}

func TestResultsListNewestFirst(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	m := h.open(storesFactory(newFakeStore(object("src/a", 1)), newFakeStore()), func(o *Options) { o.Now = clock.Now })
	for _, id := range []string{"b", "a", "c"} {
		startAndWait(t, m, requestWithID(id))
		clock.advance(1e9)
	}
	items, err := m.List()
	if err != nil || len(items) != 3 || items[0].ID != "c" || items[1].ID != "a" || items[2].ID != "b" {
		t.Fatalf("wrong order: %+v %v", items, err)
	}
	if err := m.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if items, _ = m.List(); len(items) != 2 || slices.ContainsFunc(items, func(p Progress) bool { return p.ID == "a" }) {
		t.Fatalf("deleted comparison listed: %+v", items)
	}
}

func TestResultsFailureDetailAndElapsedTime(t *testing.T) {
	h := newHarness(t)
	clock := newTestClock()
	source := newFakeStore(object("src/a", 1), object("src/b", 1), object("src/c", 1))
	// The first run lists "a", then waits for failFirst and fails; the resumed
	// run lists after "a" once resumeSecond is closed.
	failFirst, resumeSecond := make(chan struct{}), make(chan struct{})
	source.listHook = func(ctx context.Context, o transferstore.ListOptions) error {
		gate := resumeSecond
		if o.StartAfter == "" {
			if o.Cursor == "" {
				return nil
			}
			gate = failFirst
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		if gate == failFirst {
			return accessDenied()
		}
		return nil
	}
	m := h.open(storesFactory(&pagedStore{fakeStore: source, limit: 1}, newFakeStore()), func(o *Options) { o.Now = clock.Now })
	if _, err := m.Start(request()); err != nil {
		t.Fatal(err)
	}
	clock.advance(5 * time.Second)
	close(failFirst)
	waitWorkers(t, m)
	p, _ := m.Get("test", 0, 1)
	if p.Status != StateFailed || p.Error != errorFailed || !p.Resumable || p.ElapsedMillis != 5000 || p.Compared != 1 || p.Checkpoint != "a" || p.EndedAt.IsZero() {
		t.Fatalf("failure not recorded: %+v", p.Progress)
	}
	if !h.log.contains("comparison test failed: ") {
		t.Fatalf("failure not logged: %q", h.log.lines)
	}
	if _, err := m.Resume("test", nil); err != nil {
		t.Fatal(err)
	}
	clock.advance(3 * time.Second)
	close(resumeSecond)
	waitWorkers(t, m)
	p, _ = m.Get("test", 0, 10)
	if p.Status != StateCompleted || p.ElapsedMillis != 8000 || p.Compared != 3 || p.Rows != 3 || p.Resumes != 1 || p.Error != "" || p.ErrorDetail != "" || p.Resumable {
		t.Fatalf("resume did not continue: %+v", p.Progress)
	}
	for i, row := range p.Items {
		if row.Number != int64(i+1) || row.Key != string(rune('a'+i)) {
			t.Fatalf("rows after resume: %+v", p.Items)
		}
	}
}

// pagedStore lists at most limit objects per page.
type pagedStore struct {
	*fakeStore
	limit int
}

func (s *pagedStore) List(ctx context.Context, o transferstore.ListOptions) (transferstore.Page, error) {
	o.Limit = s.limit
	return s.fakeStore.List(ctx, o)
}

func TestResultsWriterCommitsBytesRead(t *testing.T) {
	h := newHarness(t)
	m := h.open(storesFactory(newFakeStore(), newFakeStore()))
	rec := runRecord{Progress: Progress{Request: requestWithID("bytes"), Status: StateRunning, ResultsProtocol: Protocol}, Directory: resultDirectory("bytes")}
	w, err := m.openWriter(rec, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.file.Close()
	rec.BytesRead = 100
	w.begin(rec)
	w.addBytes(20)
	w.addBytes(-5)
	if err := w.emit(Row{Key: "a", Status: StatusDifferent, Reasons: []string{ReasonSize}}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	err = w.flushLocked()
	w.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.record("bytes"); got.BytesRead != 120 || got.Rows != 1 || got.Different != 1 {
		t.Fatalf("bytes read not committed: %+v", got.Progress)
	}
	if err := w.emit(Row{Key: "a", Status: StatusDifferent}); err == nil {
		t.Fatal("repeated key accepted")
	}
}
