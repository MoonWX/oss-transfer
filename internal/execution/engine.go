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
	parentID   int64
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}
}

// Engine owns task lifetimes, independently of the short-lived RPC contexts
// passed to Accept and Start. One Engine may own a given Agent database at a time.
type Engine struct {
	repo    *Repository
	factory StoreFactory
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	slots   chan struct{}
	mu      sync.Mutex
	active  map[string]*activeAttempt
	closed  bool
}

func (e *Engine) Concurrency() (active, capacity int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.active), e.opts.MaxConcurrentJobs
}

func NewEngine(repo *Repository, factory StoreFactory, opts Options) (*Engine, error) {
	if repo == nil || factory == nil {
		return nil, errors.New("repository and storage factory are required")
	}
	if opts.MaxConcurrentJobs == 0 {
		opts.MaxConcurrentJobs = 4
	}
	if opts.WorkersPerJob == 0 {
		opts.WorkersPerJob = 4
	}
	if opts.QueueSize == 0 {
		opts.QueueSize = 16
	}
	if opts.CommitBatchSize == 0 {
		opts.CommitBatchSize = 32
	}
	if opts.CommitInterval == 0 {
		opts.CommitInterval = 250 * time.Millisecond
	}
	if opts.MaxConcurrentJobs < 1 || opts.MaxConcurrentJobs > 1024 || opts.WorkersPerJob < 1 || opts.WorkersPerJob > 128 || opts.QueueSize < 1 || opts.QueueSize > 128 || opts.CommitBatchSize < 1 || opts.CommitBatchSize > 128 || opts.CommitInterval < time.Millisecond {
		return nil, errors.New("invalid execution bounds")
	}
	if err := repo.Recover(context.Background()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{repo: repo, factory: factory, opts: opts, ctx: ctx, cancel: cancel, slots: make(chan struct{}, opts.MaxConcurrentJobs), active: make(map[string]*activeAttempt)}, nil
}

func (e *Engine) Accept(ctx context.Context, job Job) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Record{}, ErrClosed
	}
	return e.repo.Accept(ctx, job)
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
	return e.start(ctx, child, generation, false, false)
}

// Resume waits for any cancelled old attempt to exit. It rechecks completed
// object ledger entries against the target before granting them progress credit.
func (e *Engine) Resume(ctx context.Context, child string, generation uint64, retryFailed bool) error {
	return e.start(ctx, child, generation, true, retryFailed)
}
func (e *Engine) start(ctx context.Context, child string, generation uint64, resume, retryFailed bool) error {
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return ErrClosed
		}
		old := e.active[child]
		if old == nil {
			e.mu.Unlock()
			break
		}
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
		if err := wait(ctx, old.done); err != nil {
			return err
		}
	}
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctx.Done():
		return ErrClosed
	}
	keepSlot := false
	defer func() {
		if !keepSlot {
			<-e.slots
		}
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if old := e.active[child]; old != nil {
		if old.generation != generation {
			return ErrGeneration
		}
		return nil
	}
	current, err := e.repo.Get(ctx, child)
	if err != nil {
		return err
	}
	if current.Job.Generation != generation {
		return ErrGeneration
	}
	if terminal(current.Status) && !resume {
		if current.Status == Deleted {
			return ErrDeleted
		}
		return nil
	}
	record, err := e.repo.begin(ctx, child, generation, resume, retryFailed)
	if err != nil {
		return err
	}
	ioCtx, cancel := context.WithCancel(e.ctx)
	attempt := &activeAttempt{parentID: record.Job.ParentID, generation: generation, cancel: cancel, done: make(chan struct{})}
	e.active[child] = attempt
	keepSlot = true
	go e.runAttempt(ioCtx, record, attempt)
	return nil
}

func (e *Engine) runAttempt(ctx context.Context, record Record, attempt *activeAttempt) {
	var runErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = fmt.Errorf("execution panic: %v", recovered)
		}
		// This uses an independent context so graceful process shutdown records a
		// recoverable attempt. An explicit stop fence will reject the old attempt.
		_, _ = e.repo.finish(context.Background(), record.Job.ChildID, record.Job.Generation, record.Attempt, runErr)
		attempt.cancel()
		e.mu.Lock()
		delete(e.active, record.Job.ChildID)
		<-e.slots
		close(attempt.done)
		e.mu.Unlock()
	}()
	runErr = e.run(ctx, record)
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
	return e.stopParent(ctx, parentID, generation, true)
}
func (e *Engine) stopParent(ctx context.Context, parentID int64, generation uint64, deleted bool) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if err := e.repo.fenceParent(ctx, parentID, generation, deleted); err != nil {
		e.mu.Unlock()
		return err
	}
	var pending []chan struct{}
	for _, a := range e.active {
		if a.parentID == parentID {
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
	return nil
}

// ResumeParent advances the ownership generation after every old attempt exits.
// Children remain WAITING; the Master starts them as capacity becomes available.
func (e *Engine) ResumeParent(ctx context.Context, parentID int64, newGeneration uint64, retryFailed bool) error {
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
	if err := e.StopParent(ctx, parentID, p.Generation); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return e.repo.resumeParent(ctx, parentID, p.Generation, newGeneration, retryFailed)
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
	return nil
}
func wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) run(parentCtx context.Context, record Record) error {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	source, target, err := e.factory.Open(ctx, record.Job)
	if source != nil {
		defer source.Close()
	}
	if target != nil {
		defer target.Close()
	}
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	if source == nil || target == nil {
		return errors.New("storage factory returned a nil handle")
	}
	var invalid []string
	for _, result := range record.Results {
		if result.Status != ItemSucceeded && result.Status != ItemSkipped {
			continue
		}
		actual, err := target.Stat(ctx, result.Target.Key)
		if errors.Is(err, transferstore.ErrNotFound) {
			invalid = append(invalid, result.ID)
			continue
		}
		if err != nil {
			return fmt.Errorf("verify completed target %q: %w", result.Target.Key, err)
		}
		if changed(actual, result.Target) {
			invalid = append(invalid, result.ID)
		}
	}
	if len(invalid) > 0 {
		record, err = e.repo.invalidate(ctx, record.Job.ChildID, record.Job.Generation, record.Attempt, invalid)
		if err != nil {
			return fmt.Errorf("update recovery ledger: %w", err)
		}
	}
	queue := make(chan Item, e.opts.QueueSize)
	results := make(chan ItemResult, e.opts.QueueSize)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(queue)
		for i, item := range record.Job.Items {
			if record.Results[i].Status != ItemPending {
				continue
			}
			select {
			case queue <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	for range e.opts.WorkersPerJob {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case item, ok := <-queue:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					result := e.copyItem(ctx, record.Job, item, source, target)
					if ctx.Err() != nil {
						return
					}
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() { workers.Wait(); close(results) }()
	ticker := time.NewTicker(e.opts.CommitInterval)
	defer ticker.Stop()
	batch := make([]ItemResult, 0, e.opts.CommitBatchSize)
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
				flush()
				if commitErr != nil {
					return commitErr
				}
				return ctx.Err()
			}
			if commitErr == nil {
				batch = append(batch, result)
				if len(batch) >= e.opts.CommitBatchSize {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		}
	}
}

func changed(actual, saved transferstore.Object) bool {
	return actual.Key != saved.Key || actual.Size != saved.Size || (saved.ETag != "" && actual.ETag != saved.ETag) || (saved.VersionID != "" && actual.VersionID != saved.VersionID) || (!saved.ModifiedAt.IsZero() && !actual.ModifiedAt.Equal(saved.ModifiedAt))
}

func (e *Engine) copyItem(ctx context.Context, job Job, item Item, source, target transferstore.Store) (result ItemResult) {
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
	if job.ConflictPolicy == "SKIP" {
		existing, err := target.Stat(ctx, item.DestinationKey)
		if err == nil {
			result.Status, result.Target = ItemSkipped, existing
			return result
		}
		if !errors.Is(err, transferstore.ErrNotFound) {
			return fail("check destination", err)
		}
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
	write, err := target.Write(ctx, item.DestinationKey, reader, transferstore.WriteOptions{Size: item.Source.Size, Overwrite: job.ConflictPolicy == "OVERWRITE", ContentType: metadata.ContentType, ContentEncoding: metadata.ContentEncoding, ContentDisposition: metadata.ContentDisposition, CacheControl: metadata.CacheControl, Metadata: metadata.Metadata})
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
