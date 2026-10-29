package task

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const cleanupQueueBucket = "master-task-purges"

var cleanupBuckets = []string{
	"master-task-children", "master-task-outstanding", childOrderBucket, allChildOrderBucket,
	"master-task-load", "master-task-exceptions", "master-task-exceptions-hidden",
	"master-task-legacy-children", "master-task-legacy-history", "master-task-legacy-order",
	"master-task-leases",
}

type CleanupNodeProgress struct {
	Done           bool   `json:"done,omitempty"`
	Skipped        bool   `json:"skipped,omitempty"`
	RecordsDeleted int64  `json:"recordsDeleted,omitempty"`
	Error          string `json:"error,omitempty"`
}

// CleanupProgress is durable operation state. HTTP responses expose its summary
// fields, not the internal bucket cursor or node identities.
type CleanupProgress struct {
	Status              string                         `json:"status"`
	Phase               string                         `json:"phase"`
	RecordsDeleted      int64                          `json:"recordsDeleted"`
	CompletedNodes      int                            `json:"completedNodes"`
	TotalNodes          int                            `json:"totalNodes"`
	UpdatedAt           time.Time                      `json:"updatedAt"`
	Error               string                         `json:"error,omitempty"`
	Nodes               map[string]CleanupNodeProgress `json:"nodes,omitempty"`
	BucketIndex         int                            `json:"bucketIndex,omitempty"`
	After               string                         `json:"after,omitempty"`
	LocalRecordsDeleted int64                          `json:"localRecordsDeleted,omitempty"`
}

// RequestPurge records admission before returning to the caller. From this
// transaction onward restoration and late child observations are fenced.
func (r *Repository) RequestPurge(id int64) (p Parent, admitted bool, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Purged || p.Cleanup != nil {
			return nil
		}
		if !p.Deleted || !Ended(p.Status) {
			return ErrState
		}
		if err := state.RequireSchema(tx, "master", state.SchemaBackgroundCleanup); err != nil {
			return err
		}
		progress := &CleanupProgress{Status: "QUEUED", Phase: "AGENTS", UpdatedAt: r.now().UTC(), Nodes: map[string]CleanupNodeProgress{}}
		// Retired task nodes can still have historical records. Only the control
		// plane can decide whether an enrollment has been permanently revoked.
		for _, node := range p.Spec.Nodes {
			progress.Nodes[node.ID] = CleanupNodeProgress{}
		}
		progress.TotalNodes = len(progress.Nodes)
		p.Cleanup = progress
		if err := tx.Put(cleanupQueueBucket, key(id), id); err != nil {
			return err
		}
		if err := r.save(tx, p); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	return
}

func (r *Repository) PendingPurges(afterID int64, limit int) (parents []Parent, err error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("page size must be between 1 and 256")
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.ScanKeys(cleanupQueueBucket, "", key(afterID), limit, func(k string, _ int64) error {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				return err
			}
			p, err := get(tx, id)
			if err != nil {
				return err
			}
			if !p.Deleted || p.Purged || p.Cleanup == nil {
				return errors.New("invalid task cleanup queue entry")
			}
			parents = append(parents, p)
			return nil
		})
	})
	return
}

func cleanupSummary(c *CleanupProgress) error {
	c.CompletedNodes, c.TotalNodes, c.RecordsDeleted = 0, len(c.Nodes), c.LocalRecordsDeleted
	for _, node := range c.Nodes {
		if node.Done || node.Skipped {
			c.CompletedNodes++
		}
		if node.RecordsDeleted < 0 || node.RecordsDeleted > math.MaxInt64-c.RecordsDeleted {
			return errors.New("invalid cleanup record count")
		}
		c.RecordsDeleted += node.RecordsDeleted
	}
	return nil
}

// UpdatePurge commits acknowledgements and retry state without touching child
// records. Unchanged polling responses do not write or fsync the database.
func (r *Repository) UpdatePurge(id int64, fn func(*CleanupProgress) error) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if !p.Deleted || p.Purged || p.Cleanup == nil {
			return ErrState
		}
		before := *p.Cleanup
		before.Nodes = make(map[string]CleanupNodeProgress, len(p.Cleanup.Nodes))
		for id, node := range p.Cleanup.Nodes {
			before.Nodes[id] = node
		}
		if err := fn(p.Cleanup); err != nil {
			return err
		}
		c := p.Cleanup
		if c.BucketIndex != before.BucketIndex || c.After != before.After || c.LocalRecordsDeleted != before.LocalRecordsDeleted ||
			(c.Phase != "AGENTS" && c.Phase != "MASTER") || before.Phase == "MASTER" && c.Phase != "MASTER" ||
			(c.Status != "QUEUED" && c.Status != "RUNNING" && c.Status != "WAITING") || len(c.Nodes) != len(before.Nodes) {
			return ErrState
		}
		for id, prior := range before.Nodes {
			node, ok := c.Nodes[id]
			if !ok || prior.Done && !node.Done || prior.Skipped && !node.Skipped || node.RecordsDeleted < prior.RecordsDeleted {
				return ErrState
			}
		}
		if err := cleanupSummary(c); err != nil {
			return err
		}
		if c.Phase == "MASTER" && c.CompletedNodes != c.TotalNodes {
			return ErrState
		}
		c.UpdatedAt = before.UpdatedAt
		if reflect.DeepEqual(before, *c) {
			return nil
		}
		c.UpdatedAt = r.now().UTC()
		return r.save(tx, p)
	})
	return
}

// PurgePage performs at most one bounded writer transaction. The caller yields
// between pages, letting normal task writes and other cleanups make progress.
func (r *Repository) PurgePage(ctx context.Context, id int64) (p Parent, done bool, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Purged {
			done = true
			return nil
		}
		c := p.Cleanup
		if !p.Deleted || !Ended(p.Status) || c == nil || c.Phase != "MASTER" || c.CompletedNodes != c.TotalNodes {
			return ErrState
		}
		if c.BucketIndex < 0 || c.BucketIndex > len(cleanupBuckets) {
			return errors.New("invalid cleanup bucket cursor")
		}
		if c.BucketIndex < len(cleanupBuckets) {
			page, err := tx.DeletePrefixPage(ctx, cleanupBuckets[c.BucketIndex], key(id)+"/", c.After, state.DefaultCleanupBudget())
			if err != nil {
				return err
			}
			c.LocalRecordsDeleted += page.Deleted
			c.After = page.After
			if page.Done {
				c.BucketIndex++
				c.After = ""
			}
		}
		c.Status, c.Error, c.UpdatedAt = "RUNNING", "", r.now().UTC()
		if err := cleanupSummary(c); err != nil {
			return err
		}
		if c.BucketIndex == len(cleanupBuckets) {
			// No Agent can resume this parent after its acknowledgement (or an
			// enrollment revocation). The parent tombstone remains the authority;
			// historical lease grants, mount proofs, and imported parent copies
			// no longer need to remain in separate records. The corresponding
			// writers check Cleanup/Purged in the same transaction as their Put.
			for _, entry := range [][2]string{
				{"master-task-storage-proofs", strconv.FormatInt(id, 10)},
				{"master-task-legacy-original", key(id)},
			} {
				deleted, err := deleteCleanupExact(tx, entry[0], entry[1])
				if err != nil {
					return err
				}
				c.LocalRecordsDeleted += deleted
			}
			if err := cleanupSummary(c); err != nil {
				return err
			}
			p.Purged, done, c.Status = true, true, "DONE"
			if err := tx.Delete(cleanupQueueBucket, key(id)); err != nil {
				return err
			}
		}
		return r.save(tx, p)
	})
	return
}

func deleteCleanupExact(tx *state.Tx, bucket, target string) (int64, error) {
	found := false
	if err := tx.ScanKeys(bucket, target, "", 1, func(k string, _ int64) error {
		found = k == target
		return nil
	}); err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	if err := tx.Delete(bucket, target); err != nil {
		return 0, err
	}
	return 1, nil
}

// Purge is the synchronous metadata-only helper for callers that already have
// all node acknowledgements. User requests use RequestPurge and the background
// coordinator instead. Even this helper releases the writer lock every page.
func (r *Repository) Purge(id int64) (Parent, error) {
	p, _, err := r.RequestPurge(id)
	if err != nil || p.Purged {
		return p, err
	}
	if _, err := r.UpdatePurge(id, func(c *CleanupProgress) error {
		for id, node := range c.Nodes {
			node.Done = true
			c.Nodes[id] = node
		}
		c.Status, c.Phase = "RUNNING", "MASTER"
		return nil
	}); err != nil {
		return p, err
	}
	for {
		p, done, err := r.PurgePage(context.Background(), id)
		if err != nil || done {
			return p, err
		}
	}
}
