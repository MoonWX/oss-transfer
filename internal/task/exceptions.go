package task

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type Exception struct {
	ParentID        int64              `json:"parentTaskId"`
	ChildID         string             `json:"childTaskId"`
	Generation      uint64             `json:"generation"`
	Attempt         uint64             `json:"attempt"`
	Direction       string             `json:"direction"`
	Node            string             `json:"node"`
	Source          execution.Endpoint `json:"source"`
	Destination     execution.Endpoint `json:"destination"`
	Key             string             `json:"key"`
	DestinationKey  string             `json:"destinationKey"`
	Size            int64              `json:"size"`
	Error           string             `json:"error"`
	TargetCommitted bool               `json:"targetCommitted"`
	ObservedAt      time.Time          `json:"observedAt"`
	Identity        string             `json:"identity"`
	Assignment      uint64             `json:"assignment,omitempty"`
	RetryStatus     string             `json:"retryStatus,omitempty"`
	RetryCount      uint64             `json:"retryCount,omitempty"`
	RetryGeneration uint64             `json:"retryGeneration,omitempty"`
	RetryAssignment uint64             `json:"retryAssignment,omitempty"`
	RetryAttempt    uint64             `json:"retryAttempt,omitempty"`
	RetryUpdatedAt  time.Time          `json:"retryUpdatedAt,omitempty"`
	RetryError      string             `json:"retryError,omitempty"`
	RetryNode       string             `json:"retryNode,omitempty"`
}

const (
	ExceptionNotRetried  = "NOT_RETRIED"
	ExceptionPending     = "PENDING"
	ExceptionRecovered   = "RECOVERED"
	ExceptionRetryFailed = "FAILED"
	ExceptionSkipped     = "SKIPPED"
	ExceptionInterrupted = "INTERRUPTED"
)

type ExceptionFilter struct {
	ParentID                                                   int64
	Node, Direction, SourceBucket, DestinationBucket, Endpoint string
	LocalOnly                                                  bool
}

func syncExceptionIndex(tx *state.Tx, p Parent, child Child, record execution.Record, prior ...*execution.Record) error {
	prefix := childKey(p.ID, child.Job.Number) + "/"
	// Retain only objects which have failed. Normal successful transfers add no
	// extra records, and all outcome changes share the existing ledger commit.
	previous := map[string]Exception{}
	if err := tx.Scan("master-task-exceptions", prefix, "", 129, func(k string, raw json.RawMessage) error {
		var entry Exception
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		previous[k] = entry
		return nil
	}); err != nil {
		return err
	}
	if p.Purged {
		for k := range previous {
			if err := tx.Delete("master-task-exceptions", k); err != nil {
				return err
			}
		}
		return nil
	}
	if len(previous) == 0 {
		hasFailure := false
		for _, result := range record.Results {
			if result.Status == execution.ItemFailed {
				hasFailure = true
				break
			}
		}
		if !hasFailure {
			return nil
		}
	}
	items := make(map[string]execution.Item, len(record.Job.Items))
	for _, item := range record.Job.Items {
		items[item.ID] = item
	}
	for _, result := range record.Results {
		if len(previous) == 0 && result.Status != execution.ItemFailed {
			continue
		}
		k := prefix + digest(result.ID)
		entry, exists := previous[k]
		if result.Status != execution.ItemFailed && !exists {
			continue
		}
		item, ok := items[result.ID]
		if !ok {
			return errors.New("failed ledger references an unknown object")
		}
		item.Source = execution.EffectiveSource(item, result)
		if !exists {
			entry = Exception{ParentID: p.ID, ChildID: child.Job.ChildID, Generation: record.Job.Generation, Attempt: record.Attempt, Assignment: record.Job.Assignment, Direction: p.Spec.Direction, Node: child.Node.Address, Source: child.Job.Source, Destination: child.Job.Destination, Key: item.Source.Key, DestinationKey: item.DestinationKey, Size: item.Source.Size, Error: result.Error, TargetCommitted: result.TargetCommitted, ObservedAt: record.UpdatedAt, RetryStatus: ExceptionNotRetried}
			entry.Identity = exceptionFailureIdentity(record, result)
		} else {
			before := entry
			// Older indexes did not store assignment. Recover that baseline from
			// the previous durable child before interpreting a takeover as a retry.
			if entry.RetryStatus == "" && len(prior) > 0 && prior[0] != nil && prior[0].Job.Generation == entry.Generation && prior[0].Attempt == entry.Attempt {
				entry.Assignment = prior[0].Job.Assignment
				entry.RetryStatus = ExceptionNotRetried
			}
			updateExceptionRetry(&entry, record, result)
			entry.Size = item.Source.Size
			if entry.RetryCount > 0 && (entry.RetryCount != before.RetryCount || entry.RetryStatus != before.RetryStatus || !entry.RetryUpdatedAt.Equal(before.RetryUpdatedAt) || entry.RetryNode == "") {
				entry.RetryNode = child.Node.Address
			}
			if reflect.DeepEqual(before, entry) {
				continue
			}
		}
		if err := tx.Put("master-task-exceptions", k, entry); err != nil {
			return err
		}
	}
	return nil
}

func exceptionFailureIdentity(record execution.Record, result execution.ItemResult) string {
	return digest(struct {
		Generation, Assignment, Attempt uint64
		ID, Error                       string
	}{record.Job.Generation, record.Job.Assignment, record.Attempt, result.ID, result.Error})
}

func updateExceptionRetry(entry *Exception, record execution.Record, result execution.ItemResult) {
	if result.Status != execution.ItemPending && result.Status != execution.ItemSucceeded && result.Status != execution.ItemSkipped && result.Status != execution.ItemFailed {
		return
	}
	status := entry.RetryStatus
	if status == "" {
		status = ExceptionNotRetried
	}
	generation, assignment := entry.Generation, entry.Assignment
	if entry.RetryCount > 0 {
		generation, assignment = entry.RetryGeneration, entry.RetryAssignment
	}
	resolved := status == ExceptionRecovered || status == ExceptionSkipped
	if resolved && (result.Status == execution.ItemSucceeded || result.Status == execution.ItemSkipped) {
		// Other failed objects in the same child can start another generation.
		// Retaining this object's successful ledger is not another retry.
		return
	}
	newRound := record.Job.Generation > generation || record.Job.Assignment > assignment
	if status != ExceptionPending && (result.Status == execution.ItemPending || result.Status == execution.ItemSucceeded || result.Status == execution.ItemSkipped || resolved) {
		newRound = true
	}
	if result.Status == execution.ItemFailed && status != ExceptionPending && !newRound {
		// Attempt also advances on stop and restart. An unchanged failed result
		// must not turn those fences into object retries.
		lastError := entry.Error
		if status == ExceptionRetryFailed {
			lastError = entry.RetryError
		}
		if result.Error == lastError {
			return
		}
		newRound = true
	}
	if newRound {
		entry.RetryCount++
	}
	if entry.RetryCount == 0 {
		return
	}
	next := ExceptionPending
	switch result.Status {
	case execution.ItemSucceeded:
		next = ExceptionRecovered
	case execution.ItemSkipped:
		next = ExceptionSkipped
	case execution.ItemFailed:
		next = ExceptionRetryFailed
	case execution.ItemPending:
	default:
		return
	}
	errorText := ""
	if next == ExceptionRetryFailed {
		errorText = result.Error
	}
	if !newRound && status == next && entry.RetryError == errorText {
		return
	}
	entry.RetryStatus, entry.RetryError = next, errorText
	entry.RetryGeneration, entry.RetryAssignment, entry.RetryAttempt = record.Job.Generation, record.Job.Assignment, record.Attempt
	entry.RetryUpdatedAt = record.UpdatedAt
	if next == ExceptionRetryFailed {
		entry.Identity = exceptionFailureIdentity(record, result)
	}
}

// EnsureExceptionIndex upgrades an existing Go database once. All subsequent
// updates happen atomically with Observe, so HTTP queries never rescan jobs.
func (r *Repository) EnsureExceptionIndex() error {
	return r.store.Update(func(tx *state.Tx) error {
		var version int
		if _, err := tx.Get("schema", "task-exceptions", &version); err != nil {
			return err
		}
		if version == 1 {
			return nil
		}
		after := ""
		for {
			var children []Child
			last := ""
			if err := tx.Scan("master-task-children", "", after, 128, func(k string, raw json.RawMessage) error {
				var c Child
				if err := json.Unmarshal(raw, &c); err != nil {
					return err
				}
				children = append(children, c)
				last = k
				return nil
			}); err != nil {
				return err
			}
			for _, c := range children {
				if c.Record == nil {
					continue
				}
				p, err := get(tx, c.Job.ParentID)
				if err != nil {
					return err
				}
				if err := syncExceptionIndex(tx, p, c, *c.Record); err != nil {
					return err
				}
			}
			if len(children) < 128 {
				break
			}
			after = last
		}
		return tx.Put("schema", "task-exceptions", 1)
	})
}

func exceptionMatches(e Exception, f ExceptionFilter) bool {
	if f.ParentID > 0 && e.ParentID != f.ParentID || f.Node != "" && e.Node != f.Node || f.Direction != "" && e.Direction != f.Direction || f.LocalOnly && e.Direction == "OSS_TO_OSS" || f.SourceBucket != "" && e.Source.Bucket != f.SourceBucket || f.DestinationBucket != "" && e.Destination.Bucket != f.DestinationBucket {
		return false
	}
	if f.Endpoint != "" {
		endpoint := func(e execution.Endpoint) string {
			if e.Kind == "LOCAL" {
				return e.Root
			}
			return e.Ref + "/" + e.Bucket
		}
		if !strings.Contains(endpoint(e.Source), f.Endpoint) && !strings.Contains(endpoint(e.Destination), f.Endpoint) {
			return false
		}
	}
	return true
}
func visitExceptions(ctx context.Context, tx *state.Tx, filter ExceptionFilter, visit func(string, Exception) error) error {
	prefix := ""
	if filter.ParentID > 0 {
		prefix = key(filter.ParentID) + "/"
	}
	after := ""
	var parent Parent
	for {
		count := 0
		err := tx.Scan("master-task-exceptions", prefix, after, 256, func(k string, raw json.RawMessage) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			count++
			after = k
			var entry Exception
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if !exceptionMatches(entry, filter) {
				return nil
			}
			if parent.ID != entry.ParentID {
				var err error
				parent, err = get(tx, entry.ParentID)
				if err != nil {
					return err
				}
			}
			if parent.Deleted || parent.Purged {
				return nil
			}
			var hidden string
			if _, err := tx.Get("master-task-exceptions-hidden", k, &hidden); err != nil {
				return err
			}
			if hidden == entry.Identity {
				return nil
			}
			if entry.RetryStatus == "" {
				entry.RetryStatus = ExceptionNotRetried
			}
			if entry.RetryStatus != ExceptionRecovered && entry.RetryStatus != ExceptionSkipped && parent.Generation > max(entry.Generation, entry.RetryGeneration) {
				// A request can be durably accepted before the Agent is reachable.
				// It proves pending work, never a successful object outcome.
				entry.RetryStatus = ExceptionPending
				entry.RetryGeneration = parent.Generation
			}
			if entry.RetryStatus == ExceptionPending && Ended(parent.Status) {
				entry.RetryStatus = ExceptionInterrupted
			}
			return visit(k, entry)
		})
		if err != nil {
			return err
		}
		if count < 256 {
			return nil
		}
	}
}
func (r *Repository) VisitExceptions(ctx context.Context, filter ExceptionFilter, visit func(Exception) error) error {
	return r.store.View(func(tx *state.Tx) error {
		return visitExceptions(ctx, tx, filter, func(_ string, e Exception) error { return visit(e) })
	})
}
func (r *Repository) ClearExceptions(ctx context.Context, filter ExceptionFilter) (int64, error) {
	var count int64
	err := r.store.Update(func(tx *state.Tx) error {
		return visitExceptions(ctx, tx, filter, func(k string, e Exception) error {
			if err := tx.Put("master-task-exceptions-hidden", k, e.Identity); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
