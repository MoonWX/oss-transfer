package execution

import (
	"context"
	"fmt"
	"sync"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// maxGroupCommit bounds one grouped progress transaction. Each record is up
// to 128 items, so 64 records keep a transaction to a few MB.
const maxGroupCommit = 64

// commitRequest is one attempt's progress batch waiting for a durable commit.
type commitRequest struct {
	ctx        context.Context
	child      string
	generation uint64
	attempt    uint64
	results    []ItemResult
	record     Record
	err        error
	lead       bool
	wake       chan struct{}
}

// commitGroup merges concurrent progress commits of different children into
// one write transaction, so N running children share one fsync instead of
// paying N. There is no background goroutine: the first caller to find the
// group idle commits everything queued meanwhile, then hands the leader role
// to the oldest waiting caller, so every caller returns as soon as its own
// batch is durable.
//
// Only progress commits (Repository.commit) are grouped. Accept, begin,
// finish, stop and fence transactions stay separate, so their callers still
// observe their own fsync before replying. A caller's commit is durable before
// commit returns, so per-child ordering with its later finish is unchanged.
type commitGroup struct {
	mu      sync.Mutex
	queue   []*commitRequest
	running bool
	// queued is a test hook called after a request joins the queue.
	queued func(depth int)
	// batching is a test hook called by the leader before it commits a batch.
	batching func(batch []*commitRequest)
}

func (r *Repository) commit(ctx context.Context, child string, generation, attempt uint64, results []ItemResult) (Record, error) {
	req := &commitRequest{ctx: ctx, child: child, generation: generation, attempt: attempt, results: results, wake: make(chan struct{}, 1)}
	g := &r.commits
	g.mu.Lock()
	g.queue = append(g.queue, req)
	depth, hook := len(g.queue), g.queued
	leader := !g.running
	g.running = true
	g.mu.Unlock()
	if hook != nil {
		hook(depth)
	}
	if !leader {
		<-req.wake
		if !req.lead {
			return req.record, req.err
		}
	}
	// The leader's own request is at the head of the queue: either the queue
	// was empty, or the previous leader handed over to the oldest request.
	g.mu.Lock()
	batch := append([]*commitRequest(nil), g.queue[:min(len(g.queue), maxGroupCommit)]...)
	g.queue = append(g.queue[:0:0], g.queue[len(batch):]...)
	hookBatch := g.batching
	g.mu.Unlock()
	// A panic must not strand the queue: hand over and wake everyone first,
	// then re-raise it in the leader's own attempt as before grouping.
	failure := r.commitBatchSafely(batch, hookBatch)
	g.mu.Lock()
	if len(g.queue) > 0 {
		next := g.queue[0]
		next.lead = true
		next.wake <- struct{}{}
	} else {
		g.running = false
	}
	g.mu.Unlock()
	for _, other := range batch {
		if other != req {
			other.wake <- struct{}{}
		}
	}
	if failure != nil {
		panic(failure)
	}
	return req.record, req.err
}

// commitBatchSafely commits a batch and turns a panic into an error for every
// request in it. Whether anything committed is unknown, so callers fail their
// attempt; applying results again later is idempotent.
func (r *Repository) commitBatchSafely(batch []*commitRequest, hook func([]*commitRequest)) (failure any) {
	defer func() {
		if failure = recover(); failure != nil {
			for _, req := range batch {
				req.record, req.err = Record{}, fmt.Errorf("progress commit aborted: %v", failure)
			}
		}
	}()
	if hook != nil {
		hook(batch)
	}
	r.commitBatch(batch)
	return nil
}

func applyCommit(tx *state.Tx, j *Record, generation, attempt uint64, results []ItemResult) error {
	if err := checkAttempt(tx, *j, generation, attempt); err != nil {
		return err
	}
	return applyResults(j, results)
}

func (r *Repository) commitOne(req *commitRequest) {
	req.record, req.err = r.mutate(req.ctx, req.child, func(tx *state.Tx, j *Record) error {
		return applyCommit(tx, j, req.generation, req.attempt, req.results)
	})
}

// commitBatch applies each request exactly as mutate would, inside one
// transaction. A request rejected by its own checks (a stopped attempt, a
// cancelled context) changes nothing and gets its error; the others still
// commit. A storage error aborts the whole transaction, which rolls back every
// request, so each is then retried alone. Applying results is idempotent (only
// pending items take a result), so a retry after an ambiguous failure cannot
// double-count.
func (r *Repository) commitBatch(batch []*commitRequest) {
	if len(batch) == 1 {
		r.commitOne(batch[0])
		return
	}
	err := r.db.Update(func(tx *state.Tx) error {
		for _, req := range batch {
			req.record, req.err = Record{}, nil
			if err := req.ctx.Err(); err != nil {
				req.err = err
				continue
			}
			before, err := load(tx, req.child)
			if err != nil {
				req.err = err
				continue
			}
			result := before
			result.Results = append([]ItemResult(nil), before.Results...)
			if err := applyCommit(tx, &result, req.generation, req.attempt, req.results); err != nil {
				req.err = err
				continue
			}
			if err := r.put(tx, &before, &result); err != nil {
				return err
			}
			req.record = result
		}
		return nil
	})
	if err != nil {
		for _, req := range batch {
			r.commitOne(req)
		}
	}
}
