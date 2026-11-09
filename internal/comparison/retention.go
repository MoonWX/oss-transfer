package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// retentionInterval is the period of retention passes in addition to the
// passes at start-up and after every terminal state.
const retentionInterval = time.Hour

func (m *Manager) signalRetention() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) retentionLoop() {
	defer m.background.Done()
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		m.retain()
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

type retainedComparison struct {
	id       string
	started  time.Time
	ended    time.Time
	bytes    int64
	finished bool
}

// retain continues unfinished deletions, then removes finished comparisons
// that ended more than RetentionDays ago, then the finished ones that ended
// earliest while the node's results exceed BudgetBytes or more than
// MaxRetained comparisons exist. The running comparison is never removed.
// Passes are serialized.
func (m *Manager) retain() {
	m.retention.Lock()
	defer m.retention.Unlock()
	ctx := m.ctx
	if ctx.Err() != nil {
		return
	}
	var deleting []string
	var entries []retainedComparison
	err := m.db.View(func(tx *state.Tx) error {
		if err := scanBucket(tx, runsBucket, func(raw json.RawMessage) error {
			var rec runRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			if rec.Deleting {
				deleting = append(deleting, rec.ID)
				return nil
			}
			ended := rec.EndedAt
			if ended.IsZero() {
				ended = rec.UpdatedAt
			}
			stored := rec.StoredBytes
			if rec.Unavailable {
				// The results file is gone; it no longer uses the budget.
				stored = 0
			}
			entries = append(entries, retainedComparison{id: rec.ID, started: rec.StartedAt, ended: ended, bytes: stored, finished: terminalState(rec.Status)})
			return nil
		}); err != nil {
			return err
		}
		return scanBucket(tx, legacyBucket, func(raw json.RawMessage) error {
			var p Progress
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			entries = append(entries, retainedComparison{id: p.ID, started: p.StartedAt, ended: p.UpdatedAt, bytes: p.Rows * legacyRowBytes, finished: true})
			return nil
		})
	})
	if err != nil {
		m.logf("comparison retention: %s", localCause(err))
		return
	}
	for _, id := range deleting {
		if ctx.Err() != nil {
			return
		}
		m.removeComparison(ctx, id)
	}
	active := m.activeID()
	cutoff := m.now().Add(-m.retentionPeriod())
	kept := entries[:0]
	for _, entry := range entries {
		if entry.finished && entry.id != active && entry.ended.Before(cutoff) && m.removeComparison(ctx, entry.id) {
			continue
		}
		kept = append(kept, entry)
	}
	var total int64
	for _, entry := range kept {
		total += entry.bytes
	}
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].ended.Equal(kept[j].ended) {
			return kept[i].ended.Before(kept[j].ended)
		}
		if !kept[i].started.Equal(kept[j].started) {
			return kept[i].started.Before(kept[j].started)
		}
		return kept[i].id < kept[j].id
	})
	// Earliest ended first, first for the byte budget, then for the count.
	count := len(kept)
	for _, entry := range kept {
		if total <= m.options.BudgetBytes && count <= m.options.MaxRetained || ctx.Err() != nil {
			return
		}
		if entry.finished && entry.id != active && m.removeComparison(ctx, entry.id) {
			total -= entry.bytes
			count--
		}
	}
}

// removeComparison deletes a comparison for retention. It reports whether the
// comparison is gone or being deleted.
func (m *Manager) removeComparison(ctx context.Context, id string) bool {
	rec, claimed, err := m.claimDeletion(id)
	if err != nil {
		if !errors.Is(err, ErrRunning) {
			m.logf("comparison %s: cannot delete results: %s", id, localCause(err))
		}
		return false
	}
	if claimed {
		if err := m.finishDeletion(ctx, rec); err != nil && ctx.Err() == nil {
			m.logf("comparison %s: results will be removed later: %s", id, localCause(err))
		}
	}
	return true
}

// claimDeletion hides a comparison from readers in one small transaction and
// reserves its removal for the caller. A pre-Protocol 2 record moves to a
// tombstone in comparison_runs; its rows are removed afterwards. claimed is
// false when the comparison does not exist or another caller removes it.
func (m *Manager) claimDeletion(id string) (rec runRecord, claimed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		return rec, false, nil
	}
	if m.active == id {
		return rec, false, ErrRunning
	}
	if m.deleting[id] {
		return rec, false, nil
	}
	err = m.db.Update(func(tx *state.Tx) error {
		found, err := tx.Get(runsBucket, id, &rec)
		if err != nil {
			return err
		}
		if found {
			claimed = true
			if rec.Deleting {
				return nil
			}
			rec.Deleting, rec.AutoResume = true, false
			if err := tx.Put(runsBucket, id, rec); err != nil {
				return err
			}
			return releaseActive(tx, id)
		}
		var legacy Progress
		if found, err = tx.Get(legacyBucket, id, &legacy); err != nil || !found {
			return err
		}
		claimed = true
		rec = runRecord{Progress: Progress{Request: legacy.Request, StartedAt: legacy.StartedAt, UpdatedAt: legacy.UpdatedAt}, Deleting: true, Legacy: true}
		if err := tx.Put(runsBucket, id, rec); err != nil {
			return err
		}
		if err := tx.Delete(legacyBucket, id); err != nil {
			return err
		}
		return releaseActive(tx, id)
	})
	if err != nil || !claimed {
		return runRecord{}, false, err
	}
	m.deleting[id] = true
	return rec, true, nil
}

// finishDeletion removes the result directory, then the index or legacy rows
// in bounded transactions, and finally the record. An interrupted deletion is
// continued by the next retention pass.
func (m *Manager) finishDeletion(ctx context.Context, rec runRecord) error {
	defer func() {
		m.mu.Lock()
		delete(m.deleting, rec.ID)
		m.mu.Unlock()
	}()
	if !rec.Legacy && resultDirectoryPattern.MatchString(rec.Directory) {
		if err := removeAllRetry(filepath.Join(m.dir, rec.Directory)); err != nil {
			return err
		}
	}
	bucket := blocksBucket
	if rec.Legacy {
		bucket = legacyRowsBucket
	}
	after := ""
	for {
		var page state.CleanupPage
		if err := m.db.Update(func(tx *state.Tx) error {
			var err error
			page, err = tx.DeletePrefixPage(ctx, bucket, rec.ID+"/", after, state.DefaultCleanupBudget())
			return err
		}); err != nil {
			return err
		}
		if page.Done {
			break
		}
		after = page.After
	}
	return m.db.Update(func(tx *state.Tx) error {
		var current runRecord
		found, err := tx.Get(runsBucket, rec.ID, &current)
		if err != nil || !found || !current.Deleting {
			return err
		}
		return tx.Delete(runsBucket, rec.ID)
	})
}

// removeOrphans removes result directories that no record owns: a crash after
// creating one and before committing its record leaves them behind.
func (m *Manager) removeOrphans(known map[string]bool) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		m.logf("comparison results directory: %s", localCause(err))
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !resultDirectoryPattern.MatchString(name) || known[name] {
			continue
		}
		if err := removeAllRetry(filepath.Join(m.dir, name)); err != nil {
			m.logf("comparison results directory %s: %s", name, localCause(err))
		}
	}
}

// removeAllRetry removes path. Windows refuses to delete a file another handle
// still has open (a reader finishing a page, a virus scanner), so it retries
// there for a few seconds.
func removeAllRetry(path string) error {
	err := os.RemoveAll(path)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	delay := 10 * time.Millisecond
	for attempt := 0; attempt < 12 && err != nil; attempt++ {
		time.Sleep(delay)
		delay = min(2*delay, 500*time.Millisecond)
		err = os.RemoveAll(path)
	}
	return err
}

// syncDirectory makes created or removed entries of a directory durable.
// Windows has no directory fsync; NTFS journals the namespace itself.
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
