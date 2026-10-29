package state

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// CleanupBudget bounds work while holding the database writer lock. The byte
// limit counts stored bytes: compressed values never need to be inflated just
// to remove them. A single oversized record may exceed the byte limit so that
// every nonempty page can make progress. Commit/fsync latency is external to
// this work budget and remains fully durable.
type CleanupBudget struct {
	MaxRecords  int
	MaxBytes    int64
	MaxDuration time.Duration
}

func DefaultCleanupBudget() CleanupBudget {
	return CleanupBudget{MaxRecords: 2048, MaxBytes: 8 << 20, MaxDuration: 25 * time.Millisecond}
}

type CleanupPage struct {
	After   string
	Deleted int64
	Bytes   int64
	Done    bool
}

// ScanKeys has the same exclusive cursor semantics as Scan but does not decode
// or copy values. storedBytes includes the key and the encoded value. Nested
// buckets are not application records and are not visited.
func (t *Tx) ScanKeys(bucket, prefix, after string, limit int, fn func(string, int64) error) error {
	if limit <= 0 {
		return errors.New("scan limit must be positive")
	}
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return nil
	}
	c := b.Cursor()
	start := prefix
	if after > start {
		start = after
	}
	k, v := c.Seek([]byte(start))
	if after != "" && string(k) == after {
		k, v = c.Next()
	}
	p := []byte(prefix)
	for count := 0; k != nil && bytes.HasPrefix(k, p) && count < limit; {
		if v != nil {
			if err := fn(string(k), int64(len(k))+int64(len(v))); err != nil {
				return err
			}
			count++
		}
		k, v = c.Next()
	}
	return nil
}

// DeletePrefixPage removes one bounded page and returns an exclusive cursor.
// Persist that cursor with the owning cleanup state in the same transaction.
// A fresh transaction per page rebalances the tree; the cursor also avoids
// repeatedly traversing leaf pages already emptied by a previous page.
func (t *Tx) DeletePrefixPage(ctx context.Context, bucket, prefix, after string, budget CleanupBudget) (CleanupPage, error) {
	return t.deletePrefixPage(ctx, bucket, prefix, after, budget, time.Now)
}

func (t *Tx) deletePrefixPage(ctx context.Context, bucket, prefix, after string, budget CleanupBudget, now func() time.Time) (CleanupPage, error) {
	result := CleanupPage{After: after}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !t.tx.Writable() {
		return result, bolt.ErrTxNotWritable
	}
	if budget.MaxRecords <= 0 || budget.MaxBytes <= 0 || budget.MaxDuration <= 0 {
		return result, errors.New("cleanup limits must be positive")
	}
	if after != "" && !strings.HasPrefix(after, prefix) {
		return result, errors.New("cleanup cursor is outside its prefix")
	}
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		result.Done = true
		return result, nil
	}
	started := now()
	type item struct {
		key   []byte
		bytes int64
	}
	keys := make([]item, 0, min(budget.MaxRecords, 2048))
	c := b.Cursor()
	start := prefix
	if after > start {
		start = after
	}
	k, v := c.Seek([]byte(start))
	if after != "" && string(k) == after {
		k, v = c.Next()
	}
	p := []byte(prefix)
	var gathered int64
	for k != nil && bytes.HasPrefix(k, p) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if v == nil {
			return result, errors.New("cleanup prefix contains a nested bucket")
		}
		size := int64(len(k)) + int64(len(v))
		if len(keys) > 0 && (len(keys) >= budget.MaxRecords || size > budget.MaxBytes-gathered || now().Sub(started) >= budget.MaxDuration) {
			break
		}
		keys = append(keys, item{key: bytes.Clone(k), bytes: size})
		gathered += size
		k, v = c.Next()
	}
	exhausted := k == nil || !bytes.HasPrefix(k, p)
	for _, row := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.Deleted > 0 && now().Sub(started) >= budget.MaxDuration {
			break
		}
		if err := b.Delete(row.key); err != nil {
			return result, err
		}
		t.dirty = true
		result.After = string(row.key)
		result.Deleted++
		result.Bytes += row.bytes
	}
	result.Done = exhausted && result.Deleted == int64(len(keys))
	return result, nil
}
