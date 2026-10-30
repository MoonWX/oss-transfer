package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const jobsBucket = "execution_jobs"
const parentsBucket = "execution_parents"
const metaBucket = "execution_meta"
const readyBucket = "execution_ready"
const runningBucket = "execution_running"
const purgesBucket = "execution_purges"

// Database permits fault injection around real transactions. Update must be
// atomic and durable before it returns nil, as state.Store.Update is.
type Database interface {
	View(func(*state.Tx) error) error
	Update(func(*state.Tx) error) error
}
type RepositoryOptions struct{ MaxPendingJobs int64 }
type Repository struct {
	db         Database
	maxPending int64
	now        func() time.Time
}
type usage struct {
	Pending int64 `json:"pending"`
}

// Unfinished includes accepted, running and recoverable children. It uses the
// same durable admission counter as Accept, without scanning task history.
func (r *Repository) Unfinished() (count, capacity int64, err error) {
	var current usage
	err = r.db.View(func(tx *state.Tx) error { _, err := tx.Get(metaBucket, "usage", &current); return err })
	return current.Pending, r.maxPending, err
}

func NewRepository(db Database, opts RepositoryOptions) (*Repository, error) {
	if db == nil {
		return nil, errors.New("execution database is required")
	}
	if opts.MaxPendingJobs == 0 {
		opts.MaxPendingJobs = 1024
	}
	if opts.MaxPendingJobs < 1 {
		return nil, errors.New("MaxPendingJobs must be positive")
	}
	r := &Repository{db: db, maxPending: opts.MaxPendingJobs, now: time.Now}
	if err := r.ensureRunningIndex(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

func jobKey(childID string) (string, error) {
	parts := strings.Split(childID, "-")
	if len(parts) != 2 {
		return "", errors.New("child ID must be parent-number")
	}
	p, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || p < 1 {
		return "", errors.New("invalid parent ID")
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 || childID != fmt.Sprintf("%d-%d", p, n) {
		return "", errors.New("invalid child number")
	}
	return fmt.Sprintf("%020d/%020d", p, n), nil
}
func parentKey(id int64) string { return strconv.FormatInt(id, 10) }

func validate(job Job) (string, error) {
	if err := ValidateRequestLimits(job.RequestLimits); err != nil {
		return "", err
	}
	if job.ParentID < 1 || job.Number < 1 || job.ChildID != fmt.Sprintf("%d-%d", job.ParentID, job.Number) || job.Generation == 0 {
		return "", errors.New("invalid child identity or generation")
	}
	if job.ConflictPolicy != "SKIP" && job.ConflictPolicy != "OVERWRITE" {
		return "", errors.New("conflict policy must be SKIP or OVERWRITE")
	}
	if len(job.Items) > 128 {
		return "", errors.New("a child accepts at most 128 items")
	}
	ids, targets := map[string]bool{}, map[string]bool{}
	var total int64
	for _, item := range job.Items {
		if item.ID == "" || len(item.ID) > 256 || item.Source.Key == "" || item.DestinationKey == "" || len(item.Source.Key) > 32768 || len(item.DestinationKey) > 32768 || item.Source.Size < 0 || total > math.MaxInt64-item.Source.Size {
			return "", errors.New("invalid item identity, key or size")
		}
		if ids[item.ID] || targets[item.DestinationKey] {
			return "", errors.New("duplicate item ID or destination key")
		}
		ids[item.ID], targets[item.DestinationKey] = true, true
		total += item.Source.Size
	}
	// The generation is the ownership fence, not the identity of the work.
	job.Generation = 0
	job.Assignment = 0
	job.OwnerEpoch = 0
	job.DiscoveryComplete = false
	data, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	if len(data) > 4<<20 {
		return "", errors.New("job payload exceeds 4 MiB")
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

func load(tx *state.Tx, child string) (Record, error) {
	key, err := jobKey(child)
	if err != nil {
		return Record{}, err
	}
	var r Record
	ok, err := tx.Get(jobsBucket, key, &r)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}
func parent(tx *state.Tx, id int64) (ParentFence, bool, error) {
	var p ParentFence
	ok, err := tx.Get(parentsBucket, parentKey(id), &p)
	return p, ok, err
}
func checkParent(tx *state.Tx, id int64, generation uint64) error {
	p, ok, err := parent(tx, id)
	if err != nil {
		return err
	}
	if !ok || p.Generation != generation {
		return ErrGeneration
	}
	if p.Deleted {
		return ErrDeleted
	}
	if p.Stopped {
		return ErrStopped
	}
	return nil
}
func checkAttempt(tx *state.Tx, r Record, generation, attempt uint64) error {
	if err := checkParent(tx, r.Job.ParentID, generation); err != nil {
		return err
	}
	if r.Job.Generation != generation || r.Attempt != attempt || r.Status != Running {
		return ErrAttempt
	}
	return nil
}

func (r *Repository) put(tx *state.Tx, before *Record, after *Record) error {
	// Replayed seals, stops and result acknowledgements are common during
	// polling/recovery. Do not turn an unchanged record into a write merely to
	// advance UpdatedAt: each record includes the complete object assignment.
	if before != nil && reflect.DeepEqual(*before, *after) {
		return nil
	}
	oldPending := before != nil && !terminal(before.Status)
	newPending := !terminal(after.Status)
	if oldPending != newPending {
		var u usage
		if _, err := tx.Get(metaBucket, "usage", &u); err != nil {
			return err
		}
		if newPending {
			if u.Pending >= r.maxPending {
				return ErrCapacity
			}
			u.Pending++
		} else {
			u.Pending--
		}
		if u.Pending < 0 {
			return errors.New("invalid pending job counter")
		}
		if err := tx.Put(metaBucket, "usage", u); err != nil {
			return err
		}
	}
	now := r.now().UTC()
	timing := after.ExecutionTime()
	if after.ActiveTime == nil && before != nil {
		timing = before.ExecutionTime()
	}
	timing.Transition(after.Status == Running, now)
	after.ActiveTime = &timing
	after.UpdatedAt = now
	if before == nil {
		after.CreatedAt = after.UpdatedAt
	}
	if after.Status == Running && after.StartedAt == nil {
		now := after.UpdatedAt
		after.StartedAt = &now
	}
	if terminal(after.Status) || after.Status == Stopped {
		if after.FinishedAt == nil {
			now := after.UpdatedAt
			after.FinishedAt = &now
		}
	} else {
		after.FinishedAt = nil
	}
	key, _ := jobKey(after.Job.ChildID)
	wasReady := before != nil && runnable(*before)
	if runnable(*after) && !wasReady {
		if err := tx.Put(readyBucket, key, after.Job.ChildID); err != nil {
			return err
		}
	} else if !runnable(*after) && wasReady {
		if err := tx.Delete(readyBucket, key); err != nil {
			return err
		}
	}
	wasRunning := before != nil && before.Status == Running
	if after.Status == Running && !wasRunning {
		if err := tx.Put(runningBucket, key, after.Job.ChildID); err != nil {
			return err
		}
	} else if after.Status != Running && wasRunning {
		if err := tx.Delete(runningBucket, key); err != nil {
			return err
		}
	}
	return tx.Put(jobsBucket, key, after)
}

func runnable(r Record) bool {
	return r.Status == Recoverable || (r.Status == Waiting && (r.Job.DiscoveryComplete || r.Counters.Succeeded+r.Counters.Skipped+r.Counters.Failed < r.Counters.Total))
}

// Pending visits only runnable records through an index updated in the same
// transaction as each job. Completed history and stopped jobs are never scanned.
func (r *Repository) Pending(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("pending limit must be 1..256")
	}
	afterKey := ""
	if after != "" {
		var err error
		afterKey, err = jobKey(after)
		if err != nil {
			return nil, err
		}
	}
	var out []Record
	err := r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Scan(readyBucket, "", afterKey, limit, func(_ string, raw json.RawMessage) error {
			var child string
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			job, err := load(tx, child)
			if err != nil {
				return err
			}
			if !runnable(job) {
				return errors.New("inconsistent execution ready index")
			}
			out = append(out, job)
			return nil
		})
	})
	return out, err
}

// Accept is the durable ACK boundary. It neither schedules work nor silently
// drops it when a memory queue fills. Replays cannot change the work definition.
func (r *Repository) Accept(ctx context.Context, job Job, inventorySource ...bool) (Record, error) {
	return r.accept(ctx, job, nil, inventorySource...)
}

func (r *Repository) accept(ctx context.Context, job Job, seed *Record, inventorySource ...bool) (Record, error) {
	fingerprint, err := validate(job)
	if err != nil {
		return Record{}, err
	}
	var result Record
	err = r.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, ok, err := parent(tx, job.ParentID)
		if err != nil {
			return err
		}
		if !ok {
			p = ParentFence{ParentID: job.ParentID, Generation: job.Generation}
			if err := tx.Put(parentsBucket, parentKey(job.ParentID), p); err != nil {
				return err
			}
		}
		if err := checkParent(tx, job.ParentID, job.Generation); err != nil {
			return err
		}
		existing, err := load(tx, job.ChildID)
		if err == nil {
			if existing.Job.Generation > job.Generation {
				return ErrGeneration
			}
			if existing.Job.Assignment > job.Assignment {
				return ErrAssignment
			}
			if existing.Job.Assignment < job.Assignment {
				if seed == nil {
					return ErrAssignment
				}
				result, err = importedRecord(job, seed, fingerprint, len(inventorySource) > 0 && inventorySource[0])
				if err != nil {
					return err
				}
				result.Attempt = max(result.Attempt, existing.Attempt) + 1
				return r.put(tx, &existing, &result)
			}
			if existing.Fingerprint != fingerprint {
				return ErrConflict
			}
			result = existing
			if existing.Job.Generation < job.Generation {
				result.Results = append([]ItemResult(nil), existing.Results...)
				activateGeneration(&result, p)
			}
			if job.DiscoveryComplete && !existing.Job.DiscoveryComplete {
				result.Job.DiscoveryComplete = true
				if result.Status != Running && result.Status != Stopped {
					settle(&result)
				}
			}
			return r.put(tx, &existing, &result)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if seed != nil {
			result, err = importedRecord(job, seed, fingerprint, len(inventorySource) > 0 && inventorySource[0])
			if err != nil {
				return err
			}
			return r.put(tx, nil, &result)
		}
		result = Record{InventorySource: len(inventorySource) > 0 && inventorySource[0], Job: job, Status: Waiting, Fingerprint: fingerprint, Results: make([]ItemResult, len(job.Items))}
		for i, item := range job.Items {
			result.Results[i] = ItemResult{ID: item.ID, Status: ItemPending}
		}
		recount(&result)
		return r.put(tx, nil, &result)
	})
	return result, err
}

func (r *Repository) Get(ctx context.Context, child string) (Record, error) {
	var out Record
	err := r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		out, err = load(tx, child)
		return err
	})
	return out, err
}
func (r *Repository) Parent(ctx context.Context, id int64) (ParentFence, error) {
	var out ParentFence
	err := r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, ok, err := parent(tx, id)
		out = p
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}

func (r *Repository) List(ctx context.Context, parentID int64, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("list limit must be 1..256")
	}
	prefix := ""
	if parentID > 0 {
		prefix = fmt.Sprintf("%020d/", parentID)
	}
	afterKey := ""
	if after != "" {
		var err error
		afterKey, err = jobKey(after)
		if err != nil {
			return nil, err
		}
	}
	var out []Record
	err := r.db.View(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Scan(jobsBucket, prefix, afterKey, limit, func(_ string, raw json.RawMessage) error {
			var rec Record
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			out = append(out, rec)
			return nil
		})
	})
	return out, err
}

func (r *Repository) mutate(ctx context.Context, child string, fn func(*state.Tx, *Record) error) (Record, error) {
	var result Record
	err := r.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := load(tx, child)
		if err != nil {
			return err
		}
		result = before
		// Deep copy: a failed callback cannot modify the snapshot used for accounting.
		result.Results = append([]ItemResult(nil), before.Results...)
		if err := fn(tx, &result); err != nil {
			return err
		}
		return r.put(tx, &before, &result)
	})
	return result, err
}

func (r *Repository) Seal(ctx context.Context, child string, generation uint64) (Record, error) {
	return r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if j.Job.Generation != generation {
			return ErrGeneration
		}
		if err := checkParent(tx, j.Job.ParentID, generation); err != nil {
			return err
		}
		if j.Job.DiscoveryComplete {
			return nil
		}
		j.Job.DiscoveryComplete = true
		if j.Status != Running && j.Status != Stopped {
			settle(j)
		}
		return nil
	})
}

func (r *Repository) begin(ctx context.Context, child string, generation uint64, resume, retryFailed bool) (Record, error) {
	return r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if err := checkParent(tx, j.Job.ParentID, generation); err != nil {
			return err
		}
		if j.Job.Generation < generation && resume {
			p, _, err := parent(tx, j.Job.ParentID)
			if err != nil {
				return err
			}
			activateGeneration(j, p)
		}
		if j.Job.Generation != generation {
			return ErrGeneration
		}
		if j.Status == Running {
			return ErrRunning
		}
		if j.Status == Stopped && !resume {
			return ErrStopped
		}
		if j.Status == Deleted {
			return ErrDeleted
		}
		j.Attempt++
		j.Status = Running
		j.Error = ""
		if retryFailed {
			for i := range j.Results {
				if j.Results[i].Status == ItemFailed {
					j.Results[i] = ItemResult{ID: j.Results[i].ID, Status: ItemPending, ResolvedSource: j.Results[i].ResolvedSource}
				}
			}
			recount(j)
		}
		return nil
	})
}

func (r *Repository) stop(ctx context.Context, child string, generation uint64) (Record, error) {
	return r.mutate(ctx, child, func(_ *state.Tx, j *Record) error {
		if j.Job.Generation != generation {
			return ErrGeneration
		}
		if !terminal(j.Status) && j.Status != Stopped {
			j.Attempt++
			j.Status = Stopped
		}
		return nil
	})
}

func (r *Repository) commit(ctx context.Context, child string, generation, attempt uint64, results []ItemResult) (Record, error) {
	return r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if err := checkAttempt(tx, *j, generation, attempt); err != nil {
			return err
		}
		return applyResults(j, results)
	})
}

func applyResults(j *Record, results []ItemResult) error {
	index := make(map[string]int, len(j.Results))
	for i, item := range j.Results {
		index[item.ID] = i
	}
	for _, result := range results {
		i, ok := index[result.ID]
		if !ok {
			return errors.New("unknown result item")
		}
		if result.Status != ItemSucceeded && result.Status != ItemSkipped && result.Status != ItemFailed {
			return errors.New("invalid terminal item result")
		}
		if j.Results[i].Status != ItemPending {
			continue
		}
		result.ResolvedSource = j.Results[i].ResolvedSource
		j.Results[i] = result
	}
	recount(j)
	return nil
}

// invalidate withdraws ledger credit only after a fresh target Stat has proved
// absence/change. Network errors must instead fail recovery without erasing it.
func (r *Repository) invalidate(ctx context.Context, child string, generation, attempt uint64, ids []string) (Record, error) {
	return r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if err := checkAttempt(tx, *j, generation, attempt); err != nil {
			return err
		}
		wanted := map[string]bool{}
		for _, id := range ids {
			wanted[id] = true
		}
		for i := range j.Results {
			if wanted[j.Results[i].ID] {
				j.Results[i] = ItemResult{ID: j.Results[i].ID, Status: ItemPending, ResolvedSource: j.Results[i].ResolvedSource}
			}
		}
		recount(j)
		return nil
	})
}

func (r *Repository) finish(ctx context.Context, child string, generation, attempt uint64, runErr error, finalResults ...ItemResult) (Record, error) {
	return r.mutate(ctx, child, func(tx *state.Tx, j *Record) error {
		if err := checkAttempt(tx, *j, generation, attempt); err != nil {
			return err
		}
		if runErr != nil {
			j.Status = Recoverable
			j.Error = errorText(runErr)
			return nil
		}
		// The final result batch and terminal state share one durable boundary.
		// A failed commit leaves neither progress credit nor a completed job.
		if err := applyResults(j, finalResults); err != nil {
			return err
		}
		settle(j)
		return nil
	})
}

func settle(j *Record) {
	if j.Counters.Succeeded+j.Counters.Skipped+j.Counters.Failed != j.Counters.Total {
		j.Status = Recoverable
		return
	}
	if !j.Job.DiscoveryComplete {
		j.Status = Waiting
		return
	}
	if j.Counters.Failed == 0 {
		j.Status = Completed
	} else if j.Counters.Succeeded+j.Counters.Skipped > 0 {
		j.Status = PartialFailed
	} else {
		j.Status = Failed
	}
}
func recount(j *Record) {
	c := Counters{Total: int64(len(j.Job.Items))}
	for i, item := range j.Job.Items {
		item.Source = EffectiveSource(item, j.Results[i])
		c.TotalBytes += item.Source.Size
		switch j.Results[i].Status {
		case ItemSucceeded:
			c.Succeeded++
			c.SucceededBytes += item.Source.Size
		case ItemSkipped:
			c.Skipped++
			c.SkippedBytes += item.Source.Size
		case ItemFailed:
			c.Failed++
			c.FailedBytes += item.Source.Size
		}
	}
	j.Counters = c
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}

// fenceParent also works before the first Accept and can advance a lagging
// Agent's generation after a partial cluster resume. Advancing only stops work;
// it never grants ownership or resumes an attempt. Older fences are rejected.
func (r *Repository) fenceParent(ctx context.Context, id int64, generation uint64, deleted bool, fenceOnly ...bool) error {
	if id < 1 || generation == 0 {
		return ErrGeneration
	}
	return r.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, ok, err := parent(tx, id)
		if err != nil {
			return err
		}
		if ok && generation < p.Generation {
			return ErrGeneration
		}
		if p.Deleted && !deleted {
			return ErrDeleted
		}
		if p.Deleted && deleted && p.Generation == generation {
			if p.CleanupDone {
				return nil
			}
			// Legacy tombstones lack completion progress. An empty page upgrades
			// them safely, while repeated polls leave an existing queue untouched.
			var queued uint64
			found, err := tx.Get(purgesBucket, parentKey(id), &queued)
			if err != nil || found {
				return err
			}
			return tx.Put(purgesBucket, parentKey(id), generation)
		}
		if !ok {
			p = ParentFence{ParentID: id, Generation: generation}
		}
		p.Generation = generation
		p.Stopped = true
		p.Deleted = p.Deleted || deleted
		if err := tx.Put(parentsBucket, parentKey(id), p); err != nil {
			return err
		}
		if !deleted && len(fenceOnly) > 0 && fenceOnly[0] {
			return nil
		}
		if deleted {
			// The parent fence rejects every old attempt immediately. Do not
			// rewrite large result ledgers merely to delete them after I/O exits.
			return tx.Put(purgesBucket, parentKey(id), generation)
		}
		return r.changeParentJobs(tx, id, func(j *Record) error {
			advanced := j.Job.Generation != generation
			j.Job.Generation = generation
			if !terminal(j.Status) && j.Status != Stopped {
				j.Status = Stopped
				j.Attempt++
			} else if advanced {
				j.Attempt++
			}
			return nil
		})
	})
}
func (r *Repository) resumeParent(ctx context.Context, id int64, oldGeneration, newGeneration uint64, retryFailed bool, inventorySource ...bool) error {
	return r.db.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, ok, err := parent(tx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if p.Deleted {
			return ErrDeleted
		}
		if p.Generation == newGeneration && !p.Stopped {
			return nil
		}
		if p.Generation != oldGeneration || newGeneration <= oldGeneration || !p.Stopped {
			return ErrGeneration
		}
		p.Generation = newGeneration
		p.Stopped = false
		p.RetryFailed = retryFailed
		p.InventorySource = len(inventorySource) > 0 && inventorySource[0]
		return tx.Put(parentsBucket, parentKey(id), p)
	})
}

// activateGeneration touches only the child being admitted. Failed history
// consumes queue capacity only when the Master actually redispatches that child.
func activateGeneration(j *Record, p ParentFence) {
	if j.Status == Retired {
		return
	}
	if j.Status == Running {
		// A fenced attempt without a durable stop can prove work only through
		// its last checkpoint, not the time spent waiting for readmission.
		timing := j.ExecutionTime()
		timing.Transition(false, j.UpdatedAt)
		j.ActiveTime = &timing
	}
	j.Job.Generation = p.Generation
	j.Attempt++
	j.InventorySource = j.InventorySource || p.InventorySource
	if j.Status != Completed {
		j.Status = Waiting
		j.Error = ""
	}
	if p.RetryFailed {
		for i := range j.Results {
			if j.Results[i].Status == ItemFailed {
				j.Results[i] = ItemResult{ID: j.Results[i].ID, Status: ItemPending, ResolvedSource: j.Results[i].ResolvedSource}
			}
		}
		recount(j)
	}
}

func (r *Repository) changeParentJobs(tx *state.Tx, id int64, fn func(*Record) error) error {
	prefix := fmt.Sprintf("%020d/", id)
	after := ""
	for {
		var page []Record
		var last string
		err := tx.Scan(jobsBucket, prefix, after, 128, func(key string, raw json.RawMessage) error {
			var j Record
			if err := json.Unmarshal(raw, &j); err != nil {
				return err
			}
			page = append(page, j)
			last = key
			return nil
		})
		if err != nil {
			return err
		}
		for _, before := range page {
			j := before
			j.Results = append([]ItemResult(nil), before.Results...)
			if err := fn(&j); err != nil {
				return err
			}
			if err := r.put(tx, &before, &j); err != nil {
				return err
			}
		}
		if len(page) < 128 {
			return nil
		}
		after = last
	}
}

// Recover is a startup operation. There must be only one live Engine per Agent
// database. It invalidates prior attempt tokens; stopped jobs stay stopped.
func (r *Repository) Recover(ctx context.Context) error {
	if err := r.purgePending(ctx); err != nil {
		return err
	}
	return r.recoverRunning(ctx)
}

// A live Engine resumes deletion separately; startup only fences interrupted I/O.
func (r *Repository) recoverRunning(ctx context.Context) error {
	after := ""
	for {
		page, err := r.running(ctx, after, 128)
		if err != nil {
			return err
		}
		for _, job := range page {
			if job.Status == Running {
				_, err := r.mutate(ctx, job.Job.ChildID, func(tx *state.Tx, j *Record) error {
					if j.Status != Running {
						return nil
					}
					timing := j.ExecutionTime()
					timing.Transition(false, j.UpdatedAt)
					j.ActiveTime = &timing
					j.Attempt++
					j.Status = Recoverable
					j.Error = "agent restarted before the attempt completed"
					p, _, err := parent(tx, j.Job.ParentID)
					if err != nil {
						return err
					}
					if p.Stopped || p.Deleted {
						j.Status = Stopped
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			after = job.Job.ChildID
		}
		if len(page) < 128 {
			return nil
		}
	}
}
