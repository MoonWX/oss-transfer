package transferstore

import (
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newTestActivity() (*ScanActivity, *testClock) {
	clock := &testClock{now: time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)}
	return newScanActivity(clock.Now), clock
}

// A source that reports nothing has no activity, rather than an idle one.
func TestScanActivityIsAbsentUntilASourceReports(t *testing.T) {
	var none *ScanActivity
	none.start(ScanModeSorted)
	none.read(3)
	none.Wait(ScanWaitCommit)()
	if none.Snapshot() != nil {
		t.Fatal("nil activity reported a snapshot")
	}
	activity, _ := newTestActivity()
	defer activity.Wait(ScanWaitCommit)()
	if activity.Snapshot() != nil {
		t.Fatal("activity without a reporting source reported a snapshot")
	}
}

// Durations are measured when the snapshot is taken.
func TestScanActivityMeasuresDurationsAtTheSnapshot(t *testing.T) {
	activity, clock := newTestActivity()
	activity.start(ScanModeSorted)
	activity.enter("a/b", true, 0, 0)
	activity.read(1024)
	clock.advance(2 * time.Second)
	end := activity.call()
	clock.advance(3 * time.Second)
	snapshot := activity.Snapshot()
	if d := snapshot.Directory; d == nil || d.Key != "a/b" || !d.Reading || d.Names != 1024 || d.ElapsedMillis != 5000 {
		t.Fatalf("directory: %+v", d)
	}
	if snapshot.OldestCallMillis != 3000 || snapshot.IdleMillis != 5000 || snapshot.NamesRead != 1024 {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	end()
	activity.listed(1000, 0)
	activity.consumed(entryExamined, 1)
	clock.advance(time.Second)
	snapshot = activity.Snapshot()
	if d := snapshot.Directory; d.Reading || d.Names != 1000 || d.Position != 2 || d.Examined != 1 || snapshot.OldestCallMillis != 0 || snapshot.IdleMillis != 1000 || snapshot.Examined != 1 {
		t.Fatalf("after reading: %+v %+v", snapshot, d)
	}
	activity.enter(".", false, 3, 3)
	if d := activity.Snapshot().Directory; d.Key != "" || d.Position != 3 || d.Examined != 0 {
		t.Fatalf("source directory: %+v", d)
	}
	// Entries already found or gone are done but were not examined.
	activity.enter("streamed", true, 0, 0)
	activity.read(5)
	activity.consumed(entryExamined, 1)
	activity.consumed(entryAlreadyFound, 2)
	activity.consumed(entryVanished, 3)
	if d := activity.Snapshot().Directory; !d.Reading || d.Names != 5 || d.Examined != 1 || d.Position != 4 {
		t.Fatalf("directory examined while read: %+v", d)
	}
}

// The mean covers the latest 1,024 lookups only.
func TestScanActivityAveragesTheLatestLookups(t *testing.T) {
	activity, _ := newTestActivity()
	activity.start(ScanModeSorted)
	if activity.Snapshot().LookupMeanMillis != nil {
		t.Fatal("mean reported before any lookup")
	}
	for range scanLookupSamples {
		activity.lookup(100 * time.Millisecond)
	}
	for range scanLookupSamples / 2 {
		activity.lookup(20 * time.Millisecond)
	}
	if mean := *activity.Snapshot().LookupMeanMillis; mean != 60 {
		t.Fatalf("mean %v ms, want 60", mean)
	}
}

// The scan waits for the local limit only when the operation it needs next
// is queued there; saving its results takes precedence.
func TestScanActivityReportsWhatTheScanWaitsFor(t *testing.T) {
	activity, _ := newTestActivity()
	activity.start(ScanModeSorted)
	var phase atomic.Int32
	stop := activity.await(&phase)
	if activity.Snapshot().WaitingFor != "" {
		t.Fatal("a lookup in its source call reported as waiting for the limit")
	}
	phase.Store(lookupWaitingLimit)
	if got := activity.Snapshot().WaitingFor; got != ScanWaitLocalLimit {
		t.Fatalf("waiting for %q", got)
	}
	stop()
	if activity.Snapshot().WaitingFor != "" {
		t.Fatal("waiting reported after the lookup arrived")
	}
	release := activity.limit()
	saving := activity.Wait(ScanWaitCommit)
	if got := activity.Snapshot().WaitingFor; got != ScanWaitCommit {
		t.Fatalf("waiting for %q", got)
	}
	saving()
	if got := activity.Snapshot().WaitingFor; got != ScanWaitLocalLimit {
		t.Fatalf("waiting for %q", got)
	}
	release()
	if activity.Snapshot().WaitingFor != "" {
		t.Fatal("waiting reported after the limit admitted the operation")
	}
}
