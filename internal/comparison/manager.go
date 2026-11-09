package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// ErrInvalidQuery reports invalid paging or filter input of Get and Rows.
var ErrInvalidQuery = errors.New("invalid comparison result query")

// maxUncleanResumes bounds automatic resumes after stops that recorded no
// terminal state, so a comparison that brings the process down cannot keep
// the Agent in a restart loop.
const maxUncleanResumes = 3

// maxRetentionDays keeps the retention period a valid time.Duration.
const maxRetentionDays = 36500

type Manager struct {
	mu       sync.Mutex
	db       *state.Store
	factory  execution.StoreFactory
	wrap     ReaderWrapper
	options  Options
	dir      string
	ownedDir bool
	ctx      context.Context
	cancel   context.CancelFunc
	active   string
	stop     context.CancelFunc
	writer   *resultWriter
	// workers counts comparison runs; background counts retention and the
	// deletions it continues.
	workers    sync.WaitGroup
	background sync.WaitGroup
	wake       chan struct{}
	retention  sync.Mutex
	deleting   map[string]bool
	// Set by tests before Start.
	commitHook func(*runRecord) error
	flushEvery time.Duration
}

type pendingResume struct {
	rec     runRecord
	factory execution.StoreFactory
	unclean bool
}

// New opens the comparisons recorded in db and finishes what a previous
// process left: result files are cut back to their committed length, result
// directories without a record are removed, unfinished deletions continue in
// the background, and the most recently started comparison that was running
// or was interrupted by a shutdown resumes from its checkpoint.
//
// An empty Options.Directory keeps results in a private temporary directory
// that Close removes. Without Options.ResumeFactory, automatic resumes use
// factory.
func New(db *state.Store, factory execution.StoreFactory, wrap ReaderWrapper, options Options) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{db: db, factory: factory, wrap: wrap, options: withDefaults(options), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), deleting: map[string]bool{}, flushEvery: flushInterval}
	if err := m.prepareDirectory(); err != nil {
		cancel()
		return nil, err
	}
	resume, err := m.recover()
	if err != nil {
		cancel()
		m.removeOwnedDirectory()
		return nil, err
	}
	if resume != nil {
		m.mu.Lock()
		m.autoResume(*resume)
		m.mu.Unlock()
	}
	// After the resumed comparison became active: retention never removes it.
	m.background.Add(1)
	go m.retentionLoop()
	return m, nil
}

func withDefaults(o Options) Options {
	if o.BudgetBytes <= 0 {
		o.BudgetBytes = DefaultBudgetBytes
	}
	if o.RetentionDays <= 0 {
		o.RetentionDays = DefaultRetentionDays
	}
	o.RetentionDays = min(o.RetentionDays, maxRetentionDays)
	if o.MinimumFreeBytes <= 0 {
		o.MinimumFreeBytes = DefaultMinimumFreeBytes
	}
	if o.MaxRetained <= 0 {
		o.MaxRetained = DefaultMaxRetained
	}
	o.Workers, o.ContentWorkers = max(o.Workers, 0), max(o.ContentWorkers, 0)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FreeBytes == nil {
		o.FreeBytes = freeDiskBytes
	}
	if o.Logf == nil {
		o.Logf = stderrLogf
	}
	return o
}

// stderrLogf writes one timestamped line to the os.Stderr of the moment, which
// the service host redirects to the service log after start-up.
func stderrLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (m *Manager) now() time.Time { return m.options.Now().UTC() }

func (m *Manager) logf(format string, args ...any) { m.options.Logf(format, args...) }

func (m *Manager) retentionPeriod() time.Duration {
	return time.Duration(m.options.RetentionDays) * 24 * time.Hour
}

func (m *Manager) prepareDirectory() error {
	if m.options.Directory == "" {
		dir, err := os.MkdirTemp("", "oss-transfer-comparisons-")
		if err != nil {
			return err
		}
		m.dir, m.ownedDir = dir, true
		return nil
	}
	dir, err := filepath.Abs(m.options.Directory)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	m.dir = dir
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		m.logf("comparison results directory: %s", localCause(err))
	}
	return nil
}

func (m *Manager) removeOwnedDirectory() {
	if m.ownedDir {
		if err := removeAllRetry(m.dir); err != nil {
			m.logf("comparison results directory: %s", localCause(err))
		}
	}
}

// Close stops the running comparison as INTERRUPTED (it resumes after the
// next start when possible) and waits for background work.
func (m *Manager) Close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.workers.Wait()
	m.background.Wait()
	m.removeOwnedDirectory()
}

// Start begins r, or returns the existing comparison when r was already
// started with the same request.
func (m *Manager) Start(r Request, factories ...execution.StoreFactory) (Progress, error) {
	if err := Validate(r); err != nil {
		return Progress{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Progress{}, err
	}
	existing, err := m.load(r.ID)
	if err != nil {
		return Progress{}, err
	}
	if existing.found {
		if existing.rec.Deleting {
			return Progress{}, errors.New("comparison ID is being deleted")
		}
		p := existing.progress(m)
		if p.Request != r {
			return p, errors.New("comparison ID already used")
		}
		return p, nil
	}
	if m.active != "" {
		return Progress{}, ErrBusy
	}
	factory := m.factory
	if len(factories) > 0 && factories[0] != nil {
		factory = factories[0]
	}
	if factory == nil {
		return Progress{}, errors.New("comparison storage is unavailable")
	}
	now := m.now()
	rec := runRecord{Progress: Progress{Request: r, Status: StateRunning, ResultsProtocol: Protocol, StartedAt: now, UpdatedAt: now}, Directory: resultDirectory(r.ID)}
	w, err := m.openWriter(rec, true)
	if err != nil {
		return Progress{}, err
	}
	w.begin(rec)
	if err := m.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(runsBucket, r.ID, rec); err != nil {
			return err
		}
		return tx.Put(runtimeBucket, "active", r.ID)
	}); err != nil {
		_ = w.file.Close()
		_ = removeAllRetry(filepath.Join(m.dir, rec.Directory))
		return Progress{}, err
	}
	m.launch(w, factory)
	return m.public(rec), nil
}

// Resume continues an interrupted, failed or canceled prefix comparison from
// its checkpoint with factory (nil selects the Manager's factory). Counts and
// stored rows continue. Resuming the running comparison returns its progress.
func (m *Manager) Resume(id string, factory execution.StoreFactory) (Progress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Progress{}, err
	}
	if id != "" && m.active == id && m.writer != nil {
		return m.writer.committedProgress(), nil
	}
	existing, err := m.load(id)
	if err != nil {
		return Progress{}, err
	}
	if !existing.found || existing.rec.Deleting {
		return Progress{}, ErrNotFound
	}
	if m.active != "" {
		return Progress{}, ErrBusy
	}
	rec := existing.rec
	if existing.legacy || !rec.Resumable || rec.Unavailable || rec.Scope != "prefix" || !terminalState(rec.Status) {
		return Progress{}, ErrNotResumable
	}
	if factory == nil {
		factory = m.factory
	}
	if factory == nil {
		return Progress{}, errors.New("comparison storage is unavailable")
	}
	p, err := m.resumeLocked(rec, factory, false)
	if errors.Is(err, errResultsMissing) || errors.Is(err, errResultsShort) {
		markUnavailable(&rec, err)
		if e := m.db.Update(func(tx *state.Tx) error { return tx.Put(runsBucket, rec.ID, rec) }); e != nil {
			m.logf("comparison %s: %s", rec.ID, localCause(e))
		}
		return Progress{}, ErrNotResumable
	}
	return p, err
}

func (m *Manager) resumeLocked(rec runRecord, factory execution.StoreFactory, unclean bool) (Progress, error) {
	w, err := m.openWriter(rec, false)
	if err != nil {
		return Progress{}, err
	}
	now := m.now()
	rec.Status, rec.Resumes, rec.UpdatedAt = StateRunning, rec.Resumes+1, now
	rec.Error, rec.ErrorDetail = "", ""
	rec.EndedAt, rec.ExpiresAt = time.Time{}, time.Time{}
	rec.Resumable, rec.AutoResume = false, false
	if unclean {
		rec.UncleanResumes++
	}
	w.begin(rec)
	if err := m.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(runsBucket, rec.ID, rec); err != nil {
			return err
		}
		return tx.Put(runtimeBucket, "active", rec.ID)
	}); err != nil {
		_ = w.file.Close()
		return Progress{}, err
	}
	m.launch(w, factory)
	return m.public(rec), nil
}

// autoResume resumes the comparison chosen by recover. On failure it stays
// INTERRUPTED with the reason.
func (m *Manager) autoResume(p pendingResume) {
	_, err := m.resumeLocked(p.rec, p.factory, p.unclean)
	if err == nil {
		m.logf("comparison %s resumes after its checkpoint", p.rec.ID)
		return
	}
	rec := p.rec
	if errors.Is(err, errResultsMissing) || errors.Is(err, errResultsShort) {
		markUnavailable(&rec, err)
	} else {
		rec.ErrorDetail = clip("cannot resume automatically: "+localCause(err), maxDetailBytes)
	}
	ended := rec.EndedAt
	if p.unclean {
		ended = rec.UpdatedAt
	}
	m.endAt(&rec, StateInterrupted, ended)
	m.logf("comparison %s: not resumed automatically: %s", rec.ID, rec.ErrorDetail)
	if e := m.db.Update(func(tx *state.Tx) error {
		if err := tx.Put(runsBucket, rec.ID, rec); err != nil {
			return err
		}
		return releaseActive(tx, rec.ID)
	}); e != nil {
		m.logf("comparison %s: %s", rec.ID, localCause(e))
	}
}

func (m *Manager) launch(w *resultWriter, factory execution.StoreFactory) {
	ctx, cancel := context.WithCancel(m.ctx)
	request := w.live.Request
	options := RunOptions{StartAfter: w.startAfter, Workers: m.options.Workers, OnBytes: w.addBytes}
	if request.Mode == "content" {
		options.Workers = m.options.ContentWorkers
	}
	m.active, m.stop, m.writer = request.ID, cancel, w
	m.workers.Add(1)
	go m.run(ctx, w, factory, request, options)
}

func (m *Manager) run(ctx context.Context, w *resultWriter, factory execution.StoreFactory, request Request, options RunOptions) {
	defer m.workers.Done()
	runCtx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	stop, flushed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(flushed)
		w.flushPeriodically(m.flushEvery, stop, abort)
	}()
	source, dest, err := factory.Open(runCtx, execution.Job{Source: request.Source, Destination: request.Destination})
	if err == nil {
		err = Run(runCtx, request, source, dest, m.wrap, options, w.emit)
		// A terminal result also promises that this comparison no longer owns
		// storage resources. Keep its admission slot until both ends close.
		err = errors.Join(err, dest.Close(), source.Close())
	}
	close(stop)
	<-flushed
	m.finish(ctx, w, err)
}

// finish commits the terminal state. Start, Resume, Cancel and Delete share
// m.mu: once a reader can observe the terminal commit, the next admission or
// deletion succeeds without racing this cleanup.
func (m *Manager) finish(ctx context.Context, w *resultWriter, runErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.mu.Lock()
	now := m.now()
	if w.failed == nil {
		next := &w.live
		next.Error, next.ErrorDetail = "", ""
		status := StateCompleted
		switch {
		case next.Status == StateCanceling:
			status = StateCanceled
		case ctx.Err() != nil && m.ctx.Err() != nil:
			status = StateInterrupted
		case ctx.Err() != nil:
			status = StateCanceled
		case runErr != nil:
			status = StateFailed
			next.Error, next.ErrorDetail = errorFailed, failureDetail(runErr)
		}
		m.endAt(next, status, now)
		next.AutoResume = status == StateInterrupted && next.Resumable
		if status == StateFailed {
			m.logf("comparison %s failed: %s", next.ID, detailOrUnknown(next.ErrorDetail))
		}
		_ = w.flushLocked()
	}
	if w.failed != nil {
		// Report the last committed state; rows after it were never saved.
		rec := w.committed
		rec.Error, rec.ErrorDetail = errorSave, failureDetail(w.failed)
		m.endAt(&rec, StateFailed, now)
		rec.Resumable = false
		rec.UpdatedAt, rec.BytesRead = now, w.bytesRead.Load()
		rec.ElapsedMillis = w.elapsedMillis()
		m.logf("comparison %s failed: %s", rec.ID, rec.ErrorDetail)
		if err := m.db.Update(func(tx *state.Tx) error {
			if err := tx.Put(runsBucket, rec.ID, rec); err != nil {
				return err
			}
			return releaseActive(tx, rec.ID)
		}); err != nil {
			m.logf("comparison %s: cannot record its final state: %s", rec.ID, localCause(err))
		}
	}
	if err := w.file.Close(); err != nil {
		m.logf("comparison %s: %s", w.live.ID, localCause(err))
	}
	w.mu.Unlock()
	if m.stop != nil {
		m.stop()
	}
	m.active, m.stop, m.writer = "", nil, nil
	m.signalRetention()
}

func detailOrUnknown(detail string) string {
	if detail == "" {
		return "unknown error"
	}
	return detail
}

// endAt records a terminal state reached at the given time (now when zero).
func (m *Manager) endAt(rec *runRecord, status string, at time.Time) {
	if at.IsZero() {
		at = m.now()
	}
	rec.Status, rec.EndedAt, rec.ExpiresAt = status, at, at.Add(m.retentionPeriod())
	rec.Resumable = rec.Scope == "prefix" && !rec.Unavailable && status != StateCompleted
	rec.AutoResume, rec.UncleanResumes = false, 0
}

func markUnavailable(rec *runRecord, cause error) {
	rec.Unavailable = true
	rec.Error, rec.ErrorDetail = errorUnavailable, cause.Error()
	rec.Resumable, rec.AutoResume = false, false
}

// Cancel stops the running comparison. CANCELING is committed before the run
// is stopped; the run then records CANCELED.
func (m *Manager) Cancel(id string) (Progress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" && m.active == id && m.writer != nil {
		w := m.writer
		w.mu.Lock()
		if w.live.Status == StateRunning {
			w.live.Status = StateCanceling
			if err := w.flushLocked(); err != nil {
				m.logf("comparison %s: %s", id, failureDetail(err))
			}
		}
		p := w.committed
		if p.Status != StateCanceling {
			p = w.live
		}
		w.mu.Unlock()
		m.stop()
		return m.public(p), nil
	}
	existing, err := m.load(id)
	if err != nil {
		return Progress{}, err
	}
	if !existing.found || existing.rec.Deleting {
		return Progress{}, ErrNotFound
	}
	return existing.progress(m), nil
}

// Delete removes a comparison and its results. It returns ErrRunning while
// the comparison runs and nil when it does not exist. Results that cannot be
// removed yet stay hidden and are removed by a later retention pass.
func (m *Manager) Delete(id string) error {
	rec, claimed, err := m.claimDeletion(id)
	if err != nil || !claimed {
		return err
	}
	if err := m.finishDeletion(context.Background(), rec); err != nil {
		m.logf("comparison %s: results will be removed later: %s", id, localCause(err))
	}
	return nil
}

// Get serves comparison_get: unfiltered rows after after. limit is
// 1..MaxLegacyPageRows.
func (m *Manager) Get(id string, after int64, limit int) (Page, error) {
	if after < 0 || limit < 1 || limit > MaxLegacyPageRows {
		return Page{}, fmt.Errorf("%w: after must be nonnegative and limit 1-%d", ErrInvalidQuery, MaxLegacyPageRows)
	}
	return m.page(RowsQuery{ID: id, After: after, Limit: limit})
}

// Rows serves comparison_rows: up to q.Limit rows after q.After, optionally
// filtered by status. See Page for the cursor semantics.
func (m *Manager) Rows(q RowsQuery) (Page, error) {
	if q.After < 0 || q.Limit < 1 || q.Limit > MaxPageRows || q.MaxBytes < 0 || q.MaxBytes > MaxPageBytes {
		return Page{}, fmt.Errorf("%w: after must be nonnegative, limit 1-%d and maxBytes 0-%d", ErrInvalidQuery, MaxPageRows, MaxPageBytes)
	}
	for _, status := range q.Statuses {
		if !slices.Contains(RowStatuses, status) {
			return Page{}, fmt.Errorf("%w: unknown result status", ErrInvalidQuery)
		}
	}
	return m.page(q)
}

func (m *Manager) page(q RowsQuery) (Page, error) {
	if q.ID == "" {
		return Page{}, ErrNotFound
	}
	maxBytes := q.MaxBytes
	if maxBytes == 0 {
		maxBytes = MaxPageBytes
	}
	filter := newStatusFilter(q.Statuses)
	result := Page{Items: []Row{}}
	var rec runRecord
	var blocks []indexedBlock
	fileBacked := false
	err := m.db.View(func(tx *state.Tx) error {
		found, err := tx.Get(runsBucket, q.ID, &rec)
		if err != nil {
			return err
		}
		if found {
			if rec.Deleting {
				return ErrNotFound
			}
			result.Progress = m.public(rec)
			if rec.Unavailable || q.After >= rec.Rows {
				return nil
			}
			fileBacked = true
			// A consistent committed snapshot: blocks only describe fsynced
			// bytes below rec.Committed, which never change.
			last := (rec.Rows - 1) / blockRows
			for index := q.After / blockRows; index <= last && len(blocks) < scanBlocks; index++ {
				var block resultBlock
				found, err := tx.Get(blocksBucket, blockKey(q.ID, index), &block)
				if err != nil {
					return err
				}
				if !found {
					return errResultsDamaged
				}
				blocks = append(blocks, indexedBlock{index: index, resultBlock: block})
			}
			return nil
		}
		var legacy Progress
		if found, err = tx.Get(legacyBucket, q.ID, &legacy); err != nil || !found {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		result.Progress = m.publicLegacy(legacy)
		items, last, err := legacyResultRows(tx, q, filter, maxBytes)
		if err != nil {
			return err
		}
		result.Items = items
		if last < legacy.Rows {
			result.NextAfter = last
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	if fileBacked {
		items, last, err := readResultRows(filepath.Join(m.dir, rec.Directory, resultsFile), blocks, q, filter, maxBytes)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Deleted after the snapshot was read.
				if current, e := m.load(q.ID); e == nil && (!current.found || current.rec.Deleting) {
					return Page{}, ErrNotFound
				}
			}
			return Page{}, err
		}
		result.Items = items
		if last < rec.Rows {
			result.NextAfter = last
		}
	}
	return result, nil
}

// List returns every retained comparison without rows, newest first.
func (m *Manager) List() ([]Progress, error) {
	items := []Progress{}
	err := m.db.View(func(tx *state.Tx) error {
		if err := scanBucket(tx, runsBucket, func(raw json.RawMessage) error {
			var rec runRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			if !rec.Deleting {
				items = append(items, m.public(rec))
			}
			return nil
		}); err != nil {
			return err
		}
		return scanBucket(tx, legacyBucket, func(raw json.RawMessage) error {
			var p Progress
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			items = append(items, m.publicLegacy(p))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].StartedAt.After(items[j].StartedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

// public is the Progress of a file-backed comparison.
func (m *Manager) public(rec runRecord) Progress {
	p := rec.Progress
	p.ExpiresAt = time.Time{}
	if terminalState(p.Status) && !p.EndedAt.IsZero() {
		p.ExpiresAt = p.EndedAt.Add(m.retentionPeriod())
	}
	return p
}

// publicLegacy is the Progress of a comparison stored before Protocol 2. No
// process of this version runs it, so a stored RUNNING or CANCELING state was
// interrupted.
func (m *Manager) publicLegacy(p Progress) Progress {
	p.ResultLimit, p.ResultsProtocol, p.Resumable = LegacyStoredDifferences, 0, false
	if p.Status == StateRunning || p.Status == StateCanceling {
		p.Status = StateInterrupted
	}
	if p.RowsTruncated && p.TruncatedReason == "" {
		p.TruncatedReason = TruncatedLegacyLimit
	}
	p.ExpiresAt = time.Time{}
	if !p.UpdatedAt.IsZero() {
		p.ExpiresAt = p.UpdatedAt.Add(m.retentionPeriod())
	}
	return p
}

type storedComparison struct {
	rec    runRecord
	legacy bool
	found  bool
}

func (s storedComparison) progress(m *Manager) Progress {
	if s.legacy {
		return m.publicLegacy(s.rec.Progress)
	}
	return m.public(s.rec)
}

// load reads a comparison record, including one being deleted.
func (m *Manager) load(id string) (storedComparison, error) {
	var s storedComparison
	if id == "" {
		return s, nil
	}
	err := m.db.View(func(tx *state.Tx) error {
		found, err := tx.Get(runsBucket, id, &s.rec)
		if err != nil || found {
			s.found = found
			return err
		}
		found, err = tx.Get(legacyBucket, id, &s.rec.Progress)
		s.found, s.legacy = found, found
		return err
	})
	return s, err
}

func (m *Manager) activeID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// scanBucket visits every value of bucket in key order.
func scanBucket(tx *state.Tx, bucket string, fn func(json.RawMessage) error) error {
	const page = 256
	after := ""
	for {
		visited := 0
		if err := tx.Scan(bucket, "", after, page, func(key string, raw json.RawMessage) error {
			visited++
			after = key
			return fn(raw)
		}); err != nil {
			return err
		}
		if visited < page {
			return nil
		}
	}
}

// recover repairs records left by the previous process and chooses the
// comparison to resume automatically.
func (m *Manager) recover() (*pendingResume, error) {
	var records []runRecord
	var active string
	if err := m.db.View(func(tx *state.Tx) error {
		if _, err := tx.Get(runtimeBucket, "active", &active); err != nil {
			return err
		}
		return scanBucket(tx, runsBucket, func(raw json.RawMessage) error {
			var rec runRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			records = append(records, rec)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var changed, candidates []runRecord
	for _, rec := range records {
		known[rec.Directory] = true
		if rec.Deleting {
			continue
		}
		dirty := false
		if !rec.Unavailable {
			if cause := m.checkResults(rec); cause != nil {
				markUnavailable(&rec, cause)
				m.logf("comparison %s: %s: %s", rec.ID, errorUnavailable, cause)
				dirty = true
			}
		}
		switch {
		case rec.Status == StateCanceling:
			m.endAt(&rec, StateCanceled, rec.UpdatedAt)
			dirty = true
		case rec.Status == StateRunning || rec.Status == StateInterrupted && rec.AutoResume:
			candidates = append(candidates, rec)
			continue
		}
		if dirty {
			changed = append(changed, rec)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].StartedAt.Equal(candidates[j].StartedAt) {
			return candidates[i].StartedAt.After(candidates[j].StartedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	var resume *pendingResume
	tried := false
	for _, rec := range candidates {
		unclean := rec.Status == StateRunning
		ended := rec.EndedAt
		if unclean {
			ended = rec.UpdatedAt
		}
		if !tried && rec.Scope == "prefix" && !rec.Unavailable {
			tried = true
			detail := ""
			if unclean && rec.UncleanResumes >= maxUncleanResumes {
				detail = fmt.Sprintf("not resumed automatically after %d restarts without a clean stop; resume it manually", maxUncleanResumes)
			} else if factory, err := m.resumeFactory(rec.Request); err != nil {
				detail = clip(err.Error(), maxDetailBytes)
			} else {
				resume = &pendingResume{rec: rec, factory: factory, unclean: unclean}
				continue
			}
			m.logf("comparison %s: not resumed automatically: %s", rec.ID, detail)
			m.endAt(&rec, StateInterrupted, ended)
			rec.ErrorDetail = detail
		} else {
			m.endAt(&rec, StateInterrupted, ended)
		}
		changed = append(changed, rec)
	}
	if err := m.db.Update(func(tx *state.Tx) error {
		for _, rec := range changed {
			if err := tx.Put(runsBucket, rec.ID, rec); err != nil {
				return err
			}
		}
		if active != "" && (resume == nil || resume.rec.ID != active) {
			return tx.Delete(runtimeBucket, "active")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	m.removeOrphans(known)
	return resume, nil
}

func (m *Manager) resumeFactory(r Request) (execution.StoreFactory, error) {
	if m.options.ResumeFactory != nil {
		factory, err := m.options.ResumeFactory(r)
		if err == nil && factory == nil {
			err = errors.New("comparison storage is unavailable")
		}
		return factory, err
	}
	if m.factory == nil {
		return nil, errors.New("comparison storage is unavailable")
	}
	return m.factory, nil
}

// checkResults cuts a result file back to its committed length. It reports
// results that lost committed rows.
func (m *Manager) checkResults(rec runRecord) error {
	if !resultDirectoryPattern.MatchString(rec.Directory) {
		if rec.Rows > 0 || rec.Committed > 0 {
			return errResultsMissing
		}
		return nil
	}
	file, err := os.OpenFile(filepath.Join(m.dir, rec.Directory, resultsFile), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		if rec.Rows > 0 || rec.Committed > 0 {
			return errResultsMissing
		}
		return nil
	}
	if err != nil {
		m.logf("comparison %s: cannot check results: %s", rec.ID, localCause(err))
		return nil
	}
	defer file.Close()
	if err := truncateResults(file, rec.Committed); err != nil {
		if errors.Is(err, errResultsShort) {
			return err
		}
		m.logf("comparison %s: cannot discard uncommitted results: %s", rec.ID, localCause(err))
	}
	return nil
}
