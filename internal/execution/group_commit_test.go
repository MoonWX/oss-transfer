package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// gatedDB counts write transactions and can hold the next one open, so a test
// can queue commits behind it deterministically. failChanged fails the next
// transaction that changes more than one job record.
type gatedDB struct {
	db          *state.Store
	updates     atomic.Int64
	mu          sync.Mutex
	gate        chan struct{}
	entered     chan struct{}
	failChanged atomic.Bool
}

// hold makes the next write transaction wait until the returned channel is
// closed; entered closes once that transaction is waiting.
func (d *gatedDB) hold() (release, entered chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gate, d.entered = make(chan struct{}), make(chan struct{})
	return d.gate, d.entered
}

func (d *gatedDB) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *gatedDB) Update(fn func(*state.Tx) error) error {
	d.updates.Add(1)
	d.mu.Lock()
	gate, entered := d.gate, d.entered
	d.gate = nil
	d.mu.Unlock()
	if gate != nil {
		close(entered)
		<-gate
	}
	return d.db.Update(func(tx *state.Tx) error {
		before := map[string]string{}
		_ = tx.Scan(jobsBucket, "", "", 256, func(k string, raw json.RawMessage) error { before[k] = string(raw); return nil })
		if err := fn(tx); err != nil {
			return err
		}
		changed := 0
		_ = tx.Scan(jobsBucket, "", "", 256, func(k string, raw json.RawMessage) error {
			if before[k] != string(raw) {
				changed++
			}
			return nil
		})
		if changed > 1 && d.failChanged.CompareAndSwap(true, false) {
			return errors.New("injected group commit failure")
		}
		return nil
	})
}

// runningChildren accepts and begins n children of one parent.
func runningChildren(t *testing.T, f *fixture, n int) []Record {
	t.Helper()
	var records []Record
	for i := 1; i <= n; i++ {
		job := f.job
		job.ChildID, job.Number = fmt.Sprintf("224-%d", i), i
		if _, err := f.repo.Accept(bg, job); err != nil {
			t.Fatal(err)
		}
		record, err := f.repo.begin(bg, job.ChildID, 1, false, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func succeeded(ids ...string) []ItemResult {
	var results []ItemResult
	for _, id := range ids {
		results = append(results, ItemResult{ID: id, Status: ItemSucceeded})
	}
	return results
}

// Commits queued while one is in flight share the next transaction; each
// caller returns only after its own results are durable.
func TestGroupCommitSharesOneTransactionAcrossChildren(t *testing.T) {
	f := newFixture(t, 2)
	gated := &gatedDB{db: f.db}
	f.repo, _ = NewRepository(gated, RepositoryOptions{})
	const children = 8
	records := runningChildren(t, f, children)
	gated.updates.Store(0)
	release, entered := gated.hold()
	// The first commit takes the leader role and is held inside its
	// transaction; the rest queue behind it. Release once all are queued.
	f.repo.commits.queued = func(depth int) {
		if depth == children-1 {
			close(release)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, children)
	commit := func(i int) {
		defer wg.Done()
		r := records[i]
		got, err := f.repo.commit(bg, r.Job.ChildID, 1, r.Attempt, succeeded("1"))
		if err == nil && got.Counters.Succeeded != 1 {
			err = fmt.Errorf("returned record lacks results: %+v", got.Counters)
		}
		errs[i] = err
	}
	wg.Add(1)
	go commit(0)
	<-entered
	for i := 1; i < children; i++ {
		wg.Add(1)
		go commit(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(i, err)
		}
	}
	if got := gated.updates.Load(); got != 2 {
		t.Fatalf("%d commits used %d transactions; want 2 (one in flight, one group)", children, got)
	}
	for _, r := range records {
		stored, err := f.repo.Get(bg, r.Job.ChildID)
		if err != nil || stored.Counters.Succeeded != 1 || stored.Results[0].Status != ItemSucceeded {
			t.Fatal("grouped result not durable", stored.Counters, err)
		}
	}
}

// One request's rejection does not affect the others in its group, and a
// storage failure retries each request alone without double counting.
func TestGroupCommitIsolatesRejectionsAndRetriesStorageFailures(t *testing.T) {
	for _, scenario := range []string{"stale-attempt", "storage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, 2)
			gated := &gatedDB{db: f.db}
			f.repo, _ = NewRepository(gated, RepositoryOptions{})
			records := runningChildren(t, f, 4)
			if scenario == "storage-failure" {
				gated.failChanged.Store(true)
			}
			release, entered := gated.hold()
			f.repo.commits.queued = func(depth int) {
				if depth == 3 {
					close(release)
				}
			}
			var wg sync.WaitGroup
			errs := make([]error, 4)
			commit := func(i int) {
				defer wg.Done()
				r := records[i]
				attempt := r.Attempt
				if scenario == "stale-attempt" && i == 2 {
					attempt--
				}
				_, errs[i] = f.repo.commit(bg, r.Job.ChildID, 1, attempt, succeeded("1", "2"))
			}
			wg.Add(1)
			go commit(0)
			<-entered
			for i := 1; i < 4; i++ {
				wg.Add(1)
				go commit(i)
			}
			wg.Wait()
			for i, r := range records {
				stored, err := f.repo.Get(bg, r.Job.ChildID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "stale-attempt" && i == 2 {
					if !errors.Is(errs[i], ErrAttempt) || stored.Counters.Succeeded != 0 {
						t.Fatal("stale attempt committed", errs[i], stored.Counters)
					}
					continue
				}
				if errs[i] != nil || stored.Counters.Succeeded != 2 || stored.Counters.Total != 2 {
					t.Fatal(i, errs[i], stored.Counters)
				}
			}
			if scenario == "storage-failure" && gated.failChanged.Load() {
				t.Fatal("fixture never injected the group failure")
			}
		})
	}
}

// A cancelled caller's request is rejected inside the group, like mutate.
func TestGroupCommitHonoursCancelledRequests(t *testing.T) {
	f := newFixture(t, 2)
	gated := &gatedDB{db: f.db}
	f.repo, _ = NewRepository(gated, RepositoryOptions{})
	records := runningChildren(t, f, 2)
	release, entered := gated.hold()
	f.repo.commits.queued = func(depth int) {
		if depth == 2 {
			close(release)
		}
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	var wg sync.WaitGroup
	var errFirst, errSecond, errThird error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errFirst = f.repo.commit(bg, records[0].Job.ChildID, 1, records[0].Attempt, succeeded("1"))
	}()
	<-entered
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errSecond = f.repo.commit(cancelled, records[1].Job.ChildID, 1, records[1].Attempt, succeeded("1"))
	}()
	go func() {
		defer wg.Done()
		_, errThird = f.repo.commit(bg, records[0].Job.ChildID, 1, records[0].Attempt, succeeded("2"))
	}()
	wg.Wait()
	if errFirst != nil || errThird != nil || errSecond == nil {
		t.Fatal(errFirst, errSecond, errThird)
	}
	first, _ := f.repo.Get(bg, records[0].Job.ChildID)
	second, _ := f.repo.Get(bg, records[1].Job.ChildID)
	if first.Counters.Succeeded != 2 || second.Counters.Succeeded != 0 {
		t.Fatal(first.Counters, second.Counters)
	}
}

// BenchmarkProgressCommits reports write transactions (each one fsync) per
// progress commit with 32 children committing concurrently, as running
// children do once per CommitInterval.
//
//	go test ./internal/execution -run '^$' -bench ProgressCommits
func BenchmarkProgressCommits(b *testing.B) {
	for _, grouped := range []bool{false, true} {
		name := "individual"
		if grouped {
			name = "grouped"
		}
		b.Run(name, func(b *testing.B) {
			db, err := state.Open(filepath.Join(b.TempDir(), "agent.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			gated := &gatedDB{db: db}
			repo, _ := NewRepository(gated, RepositoryOptions{})
			base := Job{ParentID: 224, Generation: 1, Source: Endpoint{Kind: "LOCAL", Root: b.TempDir()}, Destination: Endpoint{Kind: "LOCAL", Root: b.TempDir()}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true}
			const children = 32
			var records []Record
			for i := 1; i <= children; i++ {
				job := base
				job.ChildID, job.Number = fmt.Sprintf("224-%d", i), i
				for item := 1; item <= 128; item++ {
					key := fmt.Sprintf("objects/%03d/%03d", i, item)
					job.Items = append(job.Items, Item{ID: fmt.Sprint(item), Source: transferstore.Object{Key: key, Size: 1024}, DestinationKey: "copied/" + key})
				}
				if _, err := repo.Accept(bg, job); err != nil {
					b.Fatal(err)
				}
				record, err := repo.begin(bg, job.ChildID, 1, false, false)
				if err != nil {
					b.Fatal(err)
				}
				records = append(records, record)
			}
			gated.updates.Store(0)
			commits := 0
			b.ResetTimer()
			for n := range b.N {
				var wg sync.WaitGroup
				for _, r := range records {
					wg.Add(1)
					go func() {
						defer wg.Done()
						results := succeeded(fmt.Sprint(n%128 + 1))
						var err error
						if grouped {
							_, err = repo.commit(bg, r.Job.ChildID, 1, r.Attempt, results)
						} else {
							_, err = repo.mutate(bg, r.Job.ChildID, func(tx *state.Tx, j *Record) error {
								return applyCommit(tx, j, 1, r.Attempt, results)
							})
						}
						if err != nil {
							b.Error(err)
						}
					}()
				}
				wg.Wait()
				commits += children
			}
			b.StopTimer()
			b.ReportMetric(float64(gated.updates.Load())/float64(commits), "fsyncs/commit")
		})
	}
}

// A panic in the leader's batch fails that batch's callers, re-panics in the
// leader, and leaves the group usable for later commits.
func TestGroupCommitLeaderPanicDoesNotStrandQueue(t *testing.T) {
	f := newFixture(t, 2)
	gated := &gatedDB{db: f.db}
	f.repo, _ = NewRepository(gated, RepositoryOptions{})
	records := runningChildren(t, f, 4)
	release, entered := gated.hold()
	f.repo.commits.queued = func(depth int) {
		if depth == 2 {
			close(release)
		}
	}
	batches := 0
	f.repo.commits.batching = func(batch []*commitRequest) {
		batches++
		if batches == 2 {
			panic("injected")
		}
	}
	type outcome struct {
		err      error
		panicked any
	}
	results := make([]outcome, 3)
	var wg sync.WaitGroup
	commit := func(i int) {
		defer wg.Done()
		defer func() { results[i].panicked = recover() }()
		r := records[i]
		_, results[i].err = f.repo.commit(bg, r.Job.ChildID, 1, r.Attempt, succeeded("1"))
	}
	wg.Add(1)
	go commit(0)
	<-entered
	wg.Add(2)
	go commit(1)
	go commit(2)
	wg.Wait()
	if results[0].err != nil || results[0].panicked != nil {
		t.Fatalf("first leader: %+v", results[0])
	}
	leaders, failed := 0, 0
	for _, o := range results[1:] {
		if o.panicked != nil {
			leaders++
		} else if o.err != nil {
			failed++
		}
	}
	if leaders != 1 || failed != 1 {
		t.Fatalf("panicking batch outcomes %+v; want one re-panicking leader and one failed follower", results[1:])
	}
	f.repo.commits.batching = nil
	r := records[3]
	if _, err := f.repo.commit(bg, r.Job.ChildID, 1, r.Attempt, succeeded("1")); err != nil {
		t.Fatal("group stranded after panic:", err)
	}
}
