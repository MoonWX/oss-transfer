package transferstore

import (
	"sync"
	"sync/atomic"
	"time"
)

// Scan modes. A sorted directory is read completely before its entries are
// examined; a streamed one is examined while it is still being read.
const (
	ScanModeSorted    = "SORTED"
	ScanModeStreaming = "STREAMING"
)

// What a scan's progress is waiting for, besides the source storage itself.
const (
	ScanWaitLocalLimit = "LOCAL_OPERATION_LIMIT"
	ScanWaitCommit     = "COMMIT"
)

// scanLookupSamples is how many of the latest entry lookups the mean covers.
const scanLookupSamples = 1024

// Lookup phases, as seen by a scan waiting for a lookup's result.
const (
	lookupPending      int32 = iota // issued, or in a source call
	lookupWaitingLimit              // waiting for the local operation limit
	lookupFinished
)

// ScanActivity is a running scan's live progress, for display only. It is
// never persisted: its totals start from zero with each run and say what this
// run has done to the source, not how many objects the scan has found. The
// methods are safe for concurrent use and do nothing on a nil receiver, so a
// scan without an observer pays nothing.
type ScanActivity struct {
	now func() time.Time

	mu           sync.Mutex
	mode         string
	namesRead    int64
	examined     int64
	alreadyFound int64
	vanished     int64
	directory    scanDirectory
	lookups      [scanLookupSamples]time.Duration
	lookupCount  int
	lookupNext   int
	lookupSum    time.Duration
	calls        map[uint64]time.Time
	nextCall     uint64
	waits        map[string]int
	limitWaits   int           // the scan itself waits in the local limiter
	awaited      *atomic.Int32 // the phase of the lookup the scan waits for
	progress     time.Time
}

type scanDirectory struct {
	set      bool
	key      string
	reading  bool
	names    int64
	done     int64 // entries consumed, those before a resumed position included
	examined int64 // entries examined since the scan entered it
	started  time.Time
}

// ScanActivitySnapshot is ScanActivity as of one moment. Durations are
// measured at that moment, on the node that runs the scan.
type ScanActivitySnapshot struct {
	Mode         string `json:"mode"`
	NamesRead    int64  `json:"namesRead"`
	Examined     int64  `json:"examined"`
	AlreadyFound int64  `json:"alreadyFound"`
	Vanished     int64  `json:"vanished"`
	// Directory is the directory the scan is in, the deepest open one.
	Directory *ScanDirectorySnapshot `json:"directory,omitempty"`
	// LookupMeanMillis is the mean duration of the latest entry lookups,
	// absent before the first one.
	LookupMeanMillis *float64 `json:"lookupMeanMillis,omitempty"`
	// OldestCallMillis is how long the oldest source call still outstanding
	// has been waiting for the storage, zero when none is.
	OldestCallMillis int64 `json:"oldestCallMillis,omitempty"`
	// WaitingFor names what the scan waits for now, other than the source:
	// ScanWaitLocalLimit or ScanWaitCommit.
	WaitingFor string `json:"waitingFor,omitempty"`
	// IdleMillis is the time since any total last moved or a page was saved.
	IdleMillis int64 `json:"idleMillis"`
}

// ScanDirectorySnapshot describes the directory a scan is in. Key is empty
// for the source directory itself. While Reading, Names counts the names read
// so far; afterwards it is the number of distinct names, and Position is the
// 1-based number of the entry being examined. Examined counts the entries
// examined since the scan entered the directory, which a streamed directory
// does while it is still being read.
type ScanDirectorySnapshot struct {
	Key           string `json:"key"`
	Reading       bool   `json:"reading"`
	Names         int64  `json:"names"`
	Position      int64  `json:"position"`
	Examined      int64  `json:"examined"`
	ElapsedMillis int64  `json:"elapsedMillis"`
}

func NewScanActivity() *ScanActivity { return newScanActivity(time.Now) }

func newScanActivity(now func() time.Time) *ScanActivity {
	return &ScanActivity{now: now, progress: now(), calls: map[uint64]time.Time{}, waits: map[string]int{}}
}

// Snapshot returns the activity now, or nil when no source has reported any:
// a source that does not report activity must not look like one that has
// done nothing.
func (a *ScanActivity) Snapshot() *ScanActivitySnapshot {
	if a == nil {
		return nil
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode == "" {
		return nil
	}
	snapshot := &ScanActivitySnapshot{Mode: a.mode, NamesRead: a.namesRead, Examined: a.examined, AlreadyFound: a.alreadyFound,
		Vanished: a.vanished, IdleMillis: millis(now.Sub(a.progress))}
	if a.directory.set {
		d := a.directory
		snapshot.Directory = &ScanDirectorySnapshot{Key: d.key, Reading: d.reading, Names: d.names, Position: min(d.done+1, d.names),
			Examined: d.examined, ElapsedMillis: millis(now.Sub(d.started))}
	}
	if a.lookupCount > 0 {
		mean := float64(a.lookupSum) / float64(a.lookupCount) / float64(time.Millisecond)
		snapshot.LookupMeanMillis = &mean
	}
	var oldest time.Time
	for _, started := range a.calls {
		if oldest.IsZero() || started.Before(oldest) {
			oldest = started
		}
	}
	if !oldest.IsZero() {
		snapshot.OldestCallMillis = max(1, millis(now.Sub(oldest)))
	}
	// The scan waits for the local limit when it needs an operation that is
	// queued there; lookups further ahead waiting there do not hold it up.
	switch {
	case a.waits[ScanWaitCommit] > 0:
		snapshot.WaitingFor = ScanWaitCommit
	case a.limitWaits > 0 || a.awaited != nil && a.awaited.Load() == lookupWaitingLimit:
		snapshot.WaitingFor = ScanWaitLocalLimit
	}
	return snapshot
}

func millis(d time.Duration) int64 { return max(0, d.Milliseconds()) }

// Wait records that the scan waits for reason until the returned function is
// called. Discovery reports ScanWaitCommit this way.
func (a *ScanActivity) Wait(reason string) func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	a.waits[reason]++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.waits[reason]--
		a.mu.Unlock()
	}
}

// Saved records that a page reached the caller.
func (a *ScanActivity) Saved() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.progress = a.now()
	a.mu.Unlock()
}

func (a *ScanActivity) start(mode string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.mode = mode
	a.mu.Unlock()
}

// enter makes key the directory the scan is in. A directory that is still to
// be read starts with no names and reports them as they arrive; one already
// read reports its distinct names and the entry at index next it continues at.
func (a *ScanActivity) enter(key string, reading bool, names, next int) {
	if a == nil {
		return
	}
	if key == "." {
		key = ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.directory = scanDirectory{set: true, key: key, reading: reading, names: int64(names), done: int64(next), started: a.now()}
}

func (a *ScanActivity) read(n int) {
	if a == nil || n == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.namesRead += int64(n)
	a.directory.names += int64(n)
	a.progress = a.now()
}

// listed ends the reading of the current directory, which continues at the
// entry at index next.
func (a *ScanActivity) listed(names, next int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.directory.reading = false
	a.directory.names, a.directory.done = int64(names), int64(next)
}

// Outcomes of one consumed directory entry.
const (
	entryExamined = iota
	entryAlreadyFound
	entryVanished
	entryUnexamined // out of scope by its name alone
)

// consumed records one entry of the current directory as done; the scan
// continues at the entry at index next.
func (a *ScanActivity) consumed(outcome int, next int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch outcome {
	case entryExamined:
		a.examined++
		a.directory.examined++
	case entryAlreadyFound:
		a.alreadyFound++
	case entryVanished:
		a.vanished++
	}
	a.directory.done = int64(next)
	a.progress = a.now()
}

// call records one outstanding source call until the returned function runs.
func (a *ScanActivity) call() func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	id := a.nextCall
	a.nextCall++
	a.calls[id] = a.now()
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		delete(a.calls, id)
		a.mu.Unlock()
	}
}

func (a *ScanActivity) lookup(d time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lookupCount == scanLookupSamples {
		a.lookupSum -= a.lookups[a.lookupNext]
	} else {
		a.lookupCount++
	}
	a.lookups[a.lookupNext] = d
	a.lookupSum += d
	a.lookupNext = (a.lookupNext + 1) % scanLookupSamples
}

// limit records the scan itself waiting in the local limiter.
func (a *ScanActivity) limit() func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	a.limitWaits++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.limitWaits--
		a.mu.Unlock()
	}
}

// await records the scan waiting for the lookup whose phase is given.
func (a *ScanActivity) await(phase *atomic.Int32) func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	a.awaited = phase
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.awaited = nil
		a.mu.Unlock()
	}
}
