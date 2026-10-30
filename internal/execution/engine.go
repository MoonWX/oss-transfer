package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type activeAttempt struct {
	parentID       int64
	generation     uint64
	cancel         context.CancelFunc
	done           chan struct{}
	objects        int
	objectsChanged chan struct{}
	progress       *attemptProgress
}

// Engine owns task lifetimes, independently of the short-lived RPC contexts
// passed to Accept and Start. One Engine may own a given Agent database at a time.
type Engine struct {
	repo        *Repository
	factory     StoreFactory
	opts        Options
	ctx         context.Context
	cancel      context.CancelFunc
	changed     chan struct{}
	mu          sync.Mutex
	active      map[string]*activeAttempt
	lifetimes   sync.Map // child ID -> activeAttempt; cancellation never waits for DB admission.
	closed      bool
	cleanupWake chan struct{}
	cleanupDone chan struct{}
}

func (e *Engine) Concurrency() (active, capacity int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.active), e.opts.MaxConcurrentJobs
}

// CapacityChanged closes when an attempt exits or the concurrency limit changes.
// Schedulers take the channel before checking capacity to avoid losing a wakeup.
func (e *Engine) CapacityChanged() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.changed
}

func NewEngine(repo *Repository, factory StoreFactory, opts Options) (*Engine, error) {
	if repo == nil || factory == nil {
		return nil, errors.New("repository and storage factory are required")
	}
	if opts.MaxConcurrentJobs == 0 {
		opts.MaxConcurrentJobs = DefaultChildConcurrency
	}
	if opts.WorkersPerJob == 0 {
		opts.WorkersPerJob = DefaultObjectConcurrency
	}
	if opts.QueueSize == 0 {
		opts.QueueSize = 16
	}
	if opts.CommitBatchSize == 0 {
		opts.CommitBatchSize = 128
	}
	if opts.CommitInterval == 0 {
		opts.CommitInterval = time.Second
	}
	if opts.MaxConcurrentJobs < 1 || opts.MaxConcurrentJobs > 1024 || opts.WorkersPerJob < 1 || opts.WorkersPerJob > 128 || opts.QueueSize < 1 || opts.QueueSize > 128 || opts.CommitBatchSize < 1 || opts.CommitBatchSize > 128 || opts.CommitInterval < time.Millisecond {
		return nil, errors.New("invalid execution bounds")
	}
	if err := repo.recoverRunning(context.Background()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{repo: repo, factory: factory, opts: opts, ctx: ctx, cancel: cancel, changed: make(chan struct{}), active: make(map[string]*activeAttempt), cleanupWake: make(chan struct{}, 1), cleanupDone: make(chan struct{})}
	go e.cleanupLoop()
	return e, nil
}

func (e *Engine) Accept(ctx context.Context, job Job, inventorySource ...bool) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, ErrClosed
	}
	if _, err := e.jobContext(job); err != nil {
		return Record{}, err
	}
	return e.repo.Accept(ctx, job, inventorySource...)
}
func (e *Engine) Get(ctx context.Context, child string) (Record, error) {
	return e.repo.Get(ctx, child)
}
func (e *Engine) Seal(ctx context.Context, child string, generation uint64) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, ErrClosed
	}
	return e.repo.Seal(ctx, child, generation)
}

// Start returns after an execution slot and the durable attempt have been
// acquired. A full slot pool applies backpressure, without deleting the ACKed job.
func (e *Engine) Start(ctx context.Context, child string, generation uint64) error {
	return e.start(ctx, child, generation, false, false, false, false)
}

// TryStart admits waiting work without waiting behind occupied execution slots.
// Schedulers use it while holding short admission fences, such as an upgrade.
func (e *Engine) TryStart(ctx context.Context, child string, generation uint64) error {
	return e.start(ctx, child, generation, false, false, false, true)
}

// Resume waits for any cancelled old attempt to exit. It rechecks completed
// object ledger entries against the target before granting them progress credit.
func (e *Engine) Resume(ctx context.Context, child string, generation uint64, retryFailed bool) error {
	return e.start(ctx, child, generation, true, retryFailed, false, false)
}

// TryRecover only admits an interrupted job when a normal execution slot is
// free. It never revives a stopped/finished job or waits behind other transfers.
func (e *Engine) TryRecover(ctx context.Context, child string, generation uint64) error {
	return e.start(ctx, child, generation, true, false, true, true)
}

func (e *Engine) start(ctx context.Context, child string, generation uint64, resume, retryFailed, recoverOnly, nonblocking bool) error {
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return ErrClosed
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return err
		}
		if old := e.active[child]; old != nil {
			if old.generation != generation {
				e.mu.Unlock()
				return ErrGeneration
			}
			record, err := e.repo.Get(ctx, child)
			e.mu.Unlock()
			if err != nil {
				return err
			}
			if record.Status == Running {
				return nil
			}
			if !resume {
				return ErrRunning
			}
			if nonblocking {
				return ErrCapacity
			}
			if err := wait(ctx, old.done); err != nil {
				return err
			}
			continue
		}
		if len(e.active) < e.opts.MaxConcurrentJobs {
			break // Hold the lock through the durable start and slot reservation.
		}
		changed := e.changed
		e.mu.Unlock()
		if nonblocking {
			return ErrCapacity
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-e.ctx.Done():
			return ErrClosed
		}
	}
	defer e.mu.Unlock()
	current, err := e.repo.Get(ctx, child)
	if err != nil {
		return err
	}
	if current.ReleasedAssignment != nil {
		return ErrAssignment
	}
	if current.Job.Generation != generation && !(resume && current.Job.Generation < generation) {
		return ErrGeneration
	}
	if recoverOnly && current.Status != Recoverable {
		return ErrNotRecoverable
	}
	if terminal(current.Status) && !resume {
		if current.Status == Deleted {
			return ErrDeleted
		}
		return nil
	}
	admission := current.Job
	admission.Generation = generation
	parentCtx, err := e.jobContext(admission)
	if err != nil {
		return err
	}
	record, err := e.repo.begin(ctx, child, generation, resume, retryFailed)
	if err != nil {
		return err
	}
	ioCtx, cancel := context.WithCancel(e.ctx)
	stopLease := context.AfterFunc(parentCtx, cancel)
	if parentCtx.Err() != nil {
		cancel()
	}
	baseCancel := cancel
	cancel = func() { stopLease(); baseCancel() }
	attempt := &activeAttempt{parentID: record.Job.ParentID, generation: generation, cancel: cancel, done: make(chan struct{}), objectsChanged: make(chan struct{})}
	attempt.progress = newAttemptProgress(ioCtx, record)
	e.active[child] = attempt
	e.lifetimes.Store(child, attempt)
	go e.runAttempt(ioCtx, record, attempt)
	return nil
}

func (e *Engine) runAttempt(ctx context.Context, record Record, attempt *activeAttempt) {
	var runErr error
	var finalResults []ItemResult
	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = fmt.Errorf("execution panic: %v", recovered)
		}
		// This uses an independent context so graceful process shutdown records a
		// recoverable attempt. An explicit stop fence will reject the old attempt.
		_, finishErr := e.repo.finish(context.Background(), record.Job.ChildID, record.Job.Generation, record.Attempt, runErr, finalResults...)
		if finishErr != nil && runErr == nil && len(finalResults) > 0 {
			// Keep the same recovery behavior as a failed periodic result commit.
			// Stop/generation fences also reject this retry, so an old attempt
			// can never regain ownership by recording its persistence error.
			_, _ = e.repo.finish(context.Background(), record.Job.ChildID, record.Job.Generation, record.Attempt, fmt.Errorf("persist object results: %w", finishErr))
		}
		attempt.cancel()
		e.lifetimes.Delete(record.Job.ChildID)
		e.mu.Lock()
		delete(e.active, record.Job.ChildID)
		e.wakeConcurrency()
		close(attempt.done)
		e.mu.Unlock()
	}()
	finalResults, runErr = e.run(ctx, record, attempt)
}

// QuiesceParent only interrupts live work. The lease gate already prevents new
// attempts; ledgers settle as Recoverable and retain their committed results.
func (e *Engine) QuiesceParent(ctx context.Context, parentID int64) error {
	var pending []chan struct{}
	e.lifetimes.Range(func(_, value any) bool {
		a := value.(*activeAttempt)
		if a.parentID == parentID {
			a.cancel()
			pending = append(pending, a.done)
		}
		return true
	})
	for _, done := range pending {
		if err := wait(ctx, done); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Stop(ctx context.Context, child string, generation uint64) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	_, err := e.repo.stop(ctx, child, generation)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	active := e.active[child]
	if active != nil {
		active.cancel()
	}
	e.mu.Unlock()
	if active != nil {
		return wait(ctx, active.done)
	}
	return nil
}
func (e *Engine) StopParent(ctx context.Context, parentID int64, generation uint64) error {
	return e.stopParent(ctx, parentID, generation, false)
}
func (e *Engine) DeleteParent(ctx context.Context, parentID int64, generation uint64) error {
	if err := e.RequestDeleteParent(ctx, parentID, generation); err != nil {
		return err
	}
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		progress, err := e.DeletionProgress(ctx, parentID, generation)
		if err != nil || progress.Done {
			return err
		}
		if progress.Error != "" {
			return errors.New(progress.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.ctx.Done():
			return ErrClosed
		case <-timer.C:
		}
	}
}
func (e *Engine) stopParent(ctx context.Context, parentID int64, generation uint64, deleted bool, fenceOnly ...bool) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if err := e.repo.fenceParent(ctx, parentID, generation, deleted, fenceOnly...); err != nil {
		e.mu.Unlock()
		return err
	}
	var pending []chan struct{}
	var stopErr error
	for child, a := range e.active {
		if a.parentID == parentID {
			if len(fenceOnly) > 0 && fenceOnly[0] {
				// Recovery fences history lazily, but each live attempt must stop
				// its execution clock when it is canceled, before queueing again.
				_, err := e.repo.stop(ctx, child, generation)
				stopErr = errors.Join(stopErr, err)
			}
			a.cancel()
			pending = append(pending, a.done)
		}
	}
	e.mu.Unlock()
	for _, done := range pending {
		if err := wait(ctx, done); err != nil {
			return err
		}
	}
	if stopErr != nil {
		return stopErr
	}
	if deleted {
		return e.repo.purgeDeleted(ctx, parentID, generation)
	}
	return nil
}

// ResumeParent advances the ownership generation after every old attempt exits.
// Child ledgers stay untouched until the Master readmits each unfinished child.
func (e *Engine) ResumeParent(ctx context.Context, parentID int64, newGeneration uint64, retryFailed bool, inventorySource ...bool) error {
	p, err := e.repo.Parent(ctx, parentID)
	if err != nil {
		return err
	}
	if p.Deleted {
		return ErrDeleted
	}
	if newGeneration == p.Generation && !p.Stopped {
		return nil
	}
	if newGeneration <= p.Generation {
		return ErrGeneration
	}
	if err := e.stopParent(ctx, parentID, p.Generation, false, true); err != nil {
		// A replay can race with the first acknowledgement after its fence was
		// advanced. It must acknowledge that same generation, not stop it again.
		if errors.Is(err, ErrGeneration) {
			current, readErr := e.repo.Parent(ctx, parentID)
			if readErr == nil && current.Generation == newGeneration && !current.Stopped && !current.Deleted {
				return nil
			}
		}
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return e.repo.resumeParent(ctx, parentID, p.Generation, newGeneration, retryFailed, inventorySource...)
}

func (e *Engine) Wait(ctx context.Context, child string) (Record, error) {
	e.mu.Lock()
	a := e.active[child]
	e.mu.Unlock()
	if a != nil {
		if err := wait(ctx, a.done); err != nil {
			return Record{}, err
		}
	}
	return e.repo.Get(ctx, child)
}

// Close cancels and waits for all I/O. If ctx expires the Engine remains closed
// and a later Close can finish waiting; callers must not close the DB before then.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	pending := make([]chan struct{}, 0, len(e.active))
	for _, a := range e.active {
		pending = append(pending, a.done)
	}
	e.mu.Unlock()
	for _, done := range pending {
		if err := wait(ctx, done); err != nil {
			return err
		}
	}
	return wait(ctx, e.cleanupDone)
}
func wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) run(parentCtx context.Context, record Record, attempt *activeAttempt) ([]ItemResult, error) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, target, err := e.factory.Open(ctx, record.Job)
	if source != nil {
		defer source.Close()
	}
	if target != nil {
		defer target.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	if source == nil || target == nil {
		return nil, errors.New("storage factory returned a nil handle")
	}
	invalid, err := e.verifyCompleted(ctx, record, attempt, target)
	if err != nil {
		return nil, err
	}
	if len(invalid) > 0 {
		record, err = e.repo.invalidate(ctx, record.Job.ChildID, record.Job.Generation, record.Attempt, invalid)
		if err != nil {
			return nil, fmt.Errorf("update recovery ledger: %w", err)
		}
	}
	attempt.progress.initialize(record.Counters.SucceededBytes)
	results := make(chan ItemResult, e.opts.QueueSize)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i, item := range record.Job.Items {
			if record.Results[i].Status != ItemPending {
				continue
			}
			if err := e.acquireObject(ctx, attempt); err != nil {
				return
			}
			workers.Add(1)
			go func(item Item) {
				defer workers.Done()
				defer e.releaseObject(attempt)
				if ctx.Err() != nil {
					return
				}
				result := e.copyResolvedItem(ctx, record, item, source, target, attempt.progress)
				if ctx.Err() != nil {
					return
				}
				select {
				case results <- result:
				case <-ctx.Done():
				}
			}(item)
		}
	}()
	go func() { workers.Wait(); close(results) }()
	ticker := time.NewTicker(e.opts.CommitInterval)
	defer ticker.Stop()
	batch := make([]ItemResult, 0, e.opts.CommitBatchSize)
	pending := record.Counters.Total - record.Counters.Succeeded - record.Counters.Skipped - record.Counters.Failed
	var received int64
	var commitErr error
	flush := func() {
		if len(batch) == 0 || commitErr != nil || ctx.Err() != nil {
			return
		}
		_, err := e.repo.commit(ctx, record.Job.ChildID, record.Job.Generation, record.Attempt, batch)
		if err != nil {
			commitErr = fmt.Errorf("persist object results: %w", err)
			cancel()
		}
		batch = batch[:0]
	}
	for {
		select {
		case result, ok := <-results:
			if !ok {
				if commitErr != nil {
					return nil, commitErr
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return batch, nil
			}
			if commitErr == nil {
				batch = append(batch, result)
				received++
				// Once all pending objects have returned, finish persists the
				// last batch with the job state instead of rewriting it twice.
				if len(batch) >= e.opts.CommitBatchSize && received < pending {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		}
	}
}

func changed(actual, saved transferstore.Object) bool {
	return actual.Key != saved.Key || actual.Size != saved.Size || (saved.ETag != "" && !transferstore.EqualETag(actual.ETag, saved.ETag)) || (saved.VersionID != "" && actual.VersionID != saved.VersionID) || (!saved.ModifiedAt.IsZero() && !actual.ModifiedAt.Equal(saved.ModifiedAt))
}

func (e *Engine) copyItem(ctx context.Context, job Job, item Item, source, target transferstore.Store, progress ...*objectProgress) (result ItemResult) {
	result = ItemResult{ID: item.ID, Status: ItemFailed}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Error = fmt.Sprintf("object execution panic: %v", recovered)
		}
	}()
	fail := func(stage string, err error) ItemResult {
		result.Error = errorText(fmt.Errorf("%s %q: %w", stage, item.Source.Key, err))
		return result
	}
	expected := item.Source
	if job.SharedLocalSource && job.Source.Kind == "LOCAL" {
		// A verified shared mount can expose different device/inode IDs on each
		// Agent. Keep the portable discovery fingerprint; Local.Open still pins
		// the local file handle and rechecks its identity/size/mtime at EOF.
		if !strings.HasPrefix(expected.ETag, "local:") || expected.ModifiedAt.IsZero() || expected.VersionID != "" {
			return fail("open source", errors.New("shared local source requires a complete local discovery fingerprint"))
		}
		expected.ETag = ""
	}
	if item.Legacy != nil {
		if item.Legacy.Missing {
			result.Reconciliation = "SOURCE_MISSING"
			return fail("reconcile source", transferstore.ErrNotFound)
		}
		actual, err := source.Stat(ctx, item.Source.Key)
		if err != nil {
			return fail("reconcile source", err)
		}
		if changed(actual, expected) {
			return fail("reconcile source", transferstore.ErrSourceChanged)
		}
		if item.Legacy.Successful {
			verified, existing, reason, err := e.verifyLegacy(ctx, job, item, source, target, actual)
			result.Reconciliation = reason
			if err != nil {
				return fail("reconcile legacy result", err)
			}
			if verified {
				result.Status, result.Target = ItemSucceeded, existing
				return result
			}
		} else {
			result.Reconciliation = "LEGACY_REPROCESSED"
		}
	}
	destinationAbsent := false
	if job.ConflictPolicy == "SKIP" {
		existing, err := target.Stat(ctx, item.DestinationKey)
		if err == nil {
			result.Status, result.Target = ItemSkipped, existing
			return result
		}
		if !errors.Is(err, transferstore.ErrNotFound) {
			return fail("check destination", err)
		}
		destinationAbsent = true
	}
	input, metadata, err := source.Open(ctx, item.Source.Key, transferstore.OpenOptions{Expected: &expected})
	if err != nil {
		return fail("open source", err)
	}
	defer input.Close()
	// Keep a direct cancellation path to the original input even when a limiter
	// wraps it with a ReadCloser whose Close does not delegate correctly.
	stopClosing := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stopClosing()
	var reader io.ReadCloser = input
	if e.opts.DecorateReader != nil {
		reader, err = e.opts.DecorateReader(ctx, job, item, input)
		if err != nil {
			return fail("prepare reader", err)
		}
		if reader == nil {
			return fail("prepare reader", errors.New("nil decorated reader"))
		}
		defer reader.Close()
	}
	if len(progress) > 0 && progress[0] != nil {
		reader = &progressReader{ReadCloser: reader, progress: progress[0]}
	}
	write, err := target.Write(ctx, item.DestinationKey, reader, transferstore.WriteOptions{Size: item.Source.Size, Overwrite: job.ConflictPolicy == "OVERWRITE", DestinationAbsent: destinationAbsent, ContentType: metadata.ContentType, ContentEncoding: metadata.ContentEncoding, ContentDisposition: metadata.ContentDisposition, CacheControl: metadata.CacheControl, Metadata: metadata.Metadata})
	result.TargetCommitted = write.Committed
	result.Target = write.Object
	if err != nil {
		return fail("write destination", err)
	}
	if write.Skipped {
		result.Status = ItemSkipped
		return result
	}
	if !write.Committed || write.Object.Key != item.DestinationKey || write.Object.Size != item.Source.Size {
		return fail("verify write", errors.New("destination did not confirm the complete object"))
	}
	result.Status = ItemSucceeded
	return result
}
