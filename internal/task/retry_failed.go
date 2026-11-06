package task

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// Retrying the failed objects of a running task reopens ended children in
// the current generation. The Master first records that intent (BeginRetry),
// then the Agent reopens the observed ended attempt and the Master observes
// the result. Until that observation the child keeps its failed results and
// counters but counts as unfinished: the parent cannot finish, the child is
// polled again and its node is busy. An Agent that never reopened it reports
// the same ended record, whose observation restores the ledger exactly.

// retryScanBudget bounds the exception records one candidate search reads.
const retryScanBudget = 16384

var errRetryScanStop = errors.New("retry candidate found")

// RetryCandidate reports whether an ended child of the current generation
// still holds failed objects that an in-place retry may reopen.
func RetryCandidate(p Parent, child Child) bool {
	r := child.Record
	return child.Accepted && r != nil && r.Job.Generation == p.Generation && child.Job.Generation == p.Generation &&
		(r.Status == execution.Failed || r.Status == execution.PartialFailed) && r.Counters.Failed > 0 &&
		!p.HandoffPending(child.Job.Number)
}

// RetryNodeActive reports whether a node may run a reopened child: an active
// member that is not leaving. A leaving node's ended children move with its
// other unfinished work, where takeover retries their failed objects.
func (p Parent) RetryNodeActive(nodeID string) bool {
	if p.NodeRetired(nodeID) {
		return false
	}
	_, draining := p.NodeDraining(nodeID)
	return !draining
}

// RetryCandidates lists, in child order, up to limit children that
// RetryCandidate accepts, starting at an exception-index cursor ("" for the
// first page). It reads the exception index rather than child history: one
// unresolved, not already retrying record proves a candidate, after which the
// search seeks past that child. A bounded read budget can end a page early,
// with or without candidates. next continues the search and is empty once the
// index is exhausted.
func (r *Repository) RetryCandidates(id int64, cursor string, limit int) (p Parent, children []Child, next string, err error) {
	prefix := key(id) + "/"
	if limit < 1 || limit > 256 || cursor != "" && !strings.HasPrefix(cursor, prefix) {
		return p, nil, "", errors.New("invalid retry candidate page")
	}
	err = r.store.View(func(tx *state.Tx) error {
		var err error
		if p, err = get(tx, id); err != nil {
			return err
		}
		for budget := retryScanBudget; budget > 0; {
			page := min(256, budget)
			read, number := 0, 0
			err := tx.Scan("master-task-exceptions", prefix, cursor, page, func(k string, raw json.RawMessage) error {
				read++
				cursor = k
				var entry Exception
				if err := json.Unmarshal(raw, &entry); err != nil {
					return err
				}
				// A stored PENDING record belongs to a child that is retrying.
				if entry.Resolved() || entry.RetryStatus == ExceptionPending {
					return nil
				}
				n, ok := exceptionChildNumber(prefix, k)
				if !ok {
					return ErrConflict
				}
				number = n
				return errRetryScanStop
			})
			budget -= read
			if err != nil && !errors.Is(err, errRetryScanStop) {
				return err
			}
			if number == 0 {
				if read < page {
					cursor = ""
					return nil
				}
				continue
			}
			var child Child
			found, err := tx.Get("master-task-children", childKey(id, number), &child)
			if err != nil {
				return err
			}
			if found && RetryCandidate(p, child) {
				if len(children) == limit {
					// The next page starts at this child's first record.
					cursor = childKey(id, number)
					return nil
				}
				children = append(children, child)
			}
			cursor = childKey(id, number) + "/\xff"
		}
		return nil
	})
	if err != nil {
		return Parent{}, nil, "", err
	}
	return p, children, cursor, nil
}

// exceptionChildNumber parses the child number of an exception key
// (parent/child/object digest).
func exceptionChildNumber(prefix, k string) (int, bool) {
	rest, ok := strings.CutPrefix(k, prefix)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "/")
	if !ok || len(digits) != 10 {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n > 0
}

// BeginRetry durably records the intent to reopen children that are still the
// candidates the caller observed (same node and ended attempt) on an active,
// not leaving node. It returns the children it marked; the others are left
// unchanged. A stopping, resuming or changed parent refuses with ErrState or
// ErrGeneration, so a stop that is already recorded fences the retry.
func (r *Repository) BeginRetry(id int64, generation uint64, observed []Child) (marked []Child, err error) {
	err = unchangedOK(r.store.Update(func(tx *state.Tx) error {
		marked = nil
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Deleted || p.Purged || p.Cleanup != nil || p.Status != "RUNNING" || p.ResumeAfter != nil || p.FailoverPending() {
			return ErrState
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		if err := ensureChildOrder(tx, &p); err != nil {
			return err
		}
		for _, target := range observed {
			if target.Record == nil {
				continue
			}
			var child Child
			found, err := tx.Get("master-task-children", childKey(id, target.Job.Number), &child)
			if err != nil {
				return err
			}
			if !found || !RetryCandidate(p, child) || child.Record.Attempt != target.Record.Attempt ||
				child.Node.ID != target.Node.ID || !p.RetryNodeActive(child.Node.ID) {
				continue
			}
			previous := child
			// Keep the results, counters, attempt and time of the ended record:
			// only the Agent's reopened attempt may replace them.
			record := *child.Record
			countChild(&p, record.Status, -1)
			record.Status = execution.Waiting
			child.Record = &record
			if err := changePending(&p, previous, child); err != nil {
				return err
			}
			if err := syncChildOrder(tx, &p, &previous, child); err != nil {
				return err
			}
			if err := tx.Put("master-task-children", childKey(id, child.Job.Number), child); err != nil {
				return err
			}
			if err := tx.Put("master-task-outstanding", childKey(id, child.Job.Number), child.Job.Number); err != nil {
				return err
			}
			marked = append(marked, child)
		}
		if len(marked) == 0 {
			return errUnchanged
		}
		return r.save(tx, p)
	}))
	return
}
