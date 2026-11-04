package task

import (
	"encoding/json"
	"errors"
	"hash/maphash"
	"sync"
	"sync/atomic"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// summaryBucket holds a compact copy of every listed (non-purged) parent. The
// console polls the task list every second; reading these rows avoids decoding
// scan checkpoints, scheduling and failover maps and child-order state for
// every historical task. saveAt writes the row in the same transaction as the
// parent, so a committed parent and its summary never disagree.
const summaryBucket = "master-task-summaries"

// listingSummary keeps every field the task list view reads (taskView in the
// master package) and drops runtime-only state. Failover keeps only Retired,
// which ActiveNodes needs; Cleanup keeps only the fields shown to clients.
func listingSummary(p Parent) Parent {
	s := p
	s.ScanCheckpoint = nil
	s.ResumeAfter = nil
	s.Scheduling = nil
	s.ChildOrder = nil
	s.Fingerprint = ""
	s.Failover = nil
	if p.Failover != nil && len(p.Failover.Retired) > 0 {
		s.Failover = &FailoverState{Retired: p.Failover.Retired}
	}
	if p.Cleanup != nil {
		s.Cleanup = &CleanupProgress{Status: p.Cleanup.Status, Phase: p.Cleanup.Phase, RecordsDeleted: p.Cleanup.RecordsDeleted, CompletedNodes: p.Cleanup.CompletedNodes, TotalNodes: p.Cleanup.TotalNodes, UpdatedAt: p.Cleanup.UpdatedAt, Error: p.Cleanup.Error}
	}
	return s
}

// ListingSummary exposes the projection so callers can check that list rows
// built from a summary match rows built from the full parent.
func ListingSummary(p Parent) Parent { return listingSummary(p) }

func syncListingSummary(tx *state.Tx, p Parent) error {
	if p.Purged {
		return tx.Delete(summaryBucket, key(p.ID))
	}
	return tx.Put(summaryBucket, key(p.ID), listingSummary(p))
}

// EnsureListingSummaries rebuilds summary rows from the parents on every Master
// start. Older Masters do not maintain this bucket, so after a downgrade and a
// later upgrade (or an offline restore) a row may be stale or missing; no
// marker can detect that, so the rebuild is unconditional. Each page re-reads
// the parent and writes its summary in one transaction, and unchanged rows are
// not rewritten, so concurrent saves stay consistent and a repeat start costs
// one decode per listed task (the same as a single full status poll used to).
func (r *Repository) EnsureListingSummaries() error {
	for _, bucket := range []string{"master-task-visible", "master-task-recycle"} {
		after := ""
		for {
			count := 0
			err := r.store.Update(func(tx *state.Tx) error {
				return tx.Scan(bucket, "", after, 256, func(k string, raw json.RawMessage) error {
					after, count = k, count+1
					var id int64
					if err := json.Unmarshal(raw, &id); err != nil {
						return err
					}
					p, err := get(tx, id)
					if err != nil {
						return err
					}
					return syncListingSummary(tx, p)
				})
			})
			if err != nil {
				return err
			}
			if count < 256 {
				break
			}
		}
	}
	// Remove rows whose parent left both listing indexes while an older
	// binary owned the database.
	after := ""
	for {
		count := 0
		err := r.store.Update(func(tx *state.Tx) error {
			return tx.Scan(summaryBucket, "", after, 256, func(k string, _ json.RawMessage) error {
				after, count = k, count+1
				var id int64
				for _, bucket := range []string{"master-task-visible", "master-task-recycle"} {
					if found, err := tx.Get(bucket, k, &id); err != nil || found {
						return err
					}
				}
				return tx.Delete(summaryBucket, k)
			})
		})
		if err != nil {
			return err
		}
		if count < 256 {
			return nil
		}
	}
}

// summaryCache keeps decoded summary rows keyed by a hash of their stored
// bytes. Historical tasks do not change, so a repeated list poll hashes each
// stored row and decodes only rows written since the previous poll.
type summaryCache struct {
	seed maphash.Seed
	mu   sync.Mutex
	rows map[int64]cachedSummary
	// decodes counts summary rows decoded (cache misses), for measurements.
	decodes atomic.Int64
}
type cachedSummary struct {
	hash   uint64
	parent Parent
}

// maxCachedSummaries bounds memory; the default page is 10,000 rows plus the
// recycle bin. Exceeding it drops the cache, which only costs one re-decode.
const maxCachedSummaries = 25000

func newSummaryCache() *summaryCache {
	return &summaryCache{seed: maphash.MakeSeed(), rows: map[int64]cachedSummary{}}
}

func (c *summaryCache) read(tx *state.Tx, id int64) (Parent, bool, error) {
	hash, found := tx.StoredHash(summaryBucket, key(id), c.seed)
	if !found {
		return Parent{}, false, nil
	}
	c.mu.Lock()
	cached, ok := c.rows[id]
	c.mu.Unlock()
	if ok && cached.hash == hash {
		return cached.parent, true, nil
	}
	var p Parent
	if _, err := tx.Get(summaryBucket, key(id), &p); err != nil {
		return Parent{}, false, err
	}
	c.decodes.Add(1)
	c.mu.Lock()
	if len(c.rows) >= maxCachedSummaries {
		c.rows = map[int64]cachedSummary{}
	}
	c.rows[id] = cachedSummary{hash: hash, parent: p}
	c.mu.Unlock()
	return p, true, nil
}

// RecentSummaries pages the same rows as Recent but reads compact summaries,
// reusing rows decoded by earlier calls when their stored bytes are unchanged.
// Returned parents may share slices, maps and pointers with other callers and
// must be treated as read-only. A row without a summary (which only a failed
// EnsureListingSummaries can leave behind) falls back to the full parent.
func (r *Repository) RecentSummaries(before int64, limit int, deleted bool) (parents []Parent, err error) {
	return r.recent(before, limit, deleted, true)
}

func (r *Repository) recent(before int64, limit int, deleted, summaries bool) (parents []Parent, err error) {
	if before < 0 || limit < 1 || limit > 10001 {
		return nil, errors.New("invalid task page")
	}
	bucket, cursor := "master-task-visible", ""
	if deleted {
		bucket = "master-task-recycle"
	}
	if before > 0 {
		cursor = key(before)
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.ScanReverse(bucket, cursor, limit, func(_ string, raw json.RawMessage) error {
			var id int64
			if err := json.Unmarshal(raw, &id); err != nil {
				return err
			}
			if summaries && r.summaries != nil {
				p, found, err := r.summaries.read(tx, id)
				if err != nil {
					return err
				}
				if found {
					parents = append(parents, p)
					return nil
				}
			}
			p, err := get(tx, id)
			if err == nil {
				parents = append(parents, p)
			}
			return err
		})
	})
	return
}
