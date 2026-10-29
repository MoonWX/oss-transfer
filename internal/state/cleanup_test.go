package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupDoesNotDecodeValuesAndResumesWithoutTouchingNeighbors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(func(tx *Tx) error {
		b, err := tx.tx.CreateBucketIfNotExists([]byte("records"))
		if err != nil {
			return err
		}
		// A damaged compressed value is intentionally unreadable. Deleting a
		// fenced record must not require decoding its object payload.
		bad := append(bytes.Clone(compressedMagic), []byte("damaged")...)
		for i := range 40 {
			if err := b.Put([]byte(fmt.Sprintf("1/%03d", i)), bad); err != nil {
				return err
			}
		}
		tx.dirty = true
		return tx.Put("records", "10/neighbor", "keep")
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error {
		var ignored any
		if _, err := tx.Get("records", "1/000", &ignored); err == nil {
			t.Fatal("damaged fixture decoded successfully")
		}
		var count int
		if err := tx.ScanKeys("records", "1/", "", 100, func(_ string, size int64) error {
			count++
			if size <= 0 {
				t.Fatal("missing stored byte size")
			}
			return nil
		}); err != nil {
			return err
		}
		if count != 40 {
			t.Fatalf("key-only scan got %d rows", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	budget := CleanupBudget{MaxRecords: 7, MaxBytes: 1 << 20, MaxDuration: time.Second}
	var removed int64
	for pageNumber := 0; ; pageNumber++ {
		var result CleanupPage
		if err := db.Update(func(tx *Tx) error {
			var after string
			if _, err := tx.Get("cursor", "after", &after); err != nil {
				return err
			}
			result, err = tx.DeletePrefixPage(context.Background(), "records", "1/", after, budget)
			if err != nil {
				return err
			}
			return tx.Put("cursor", "after", result.After)
		}); err != nil {
			t.Fatal(err)
		}
		if result.Deleted < 1 || result.Deleted > 7 {
			t.Fatalf("unbounded or stuck page: %+v", result)
		}
		removed += result.Deleted
		if pageNumber == 1 {
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
		}
		if result.Done {
			break
		}
		// Even if the time budget yields after one record, every page must
		// make progress and this fixed fixture must finish within 40 pages.
		if pageNumber >= 39 {
			t.Fatal("cleanup did not converge")
		}
	}
	if removed != 40 {
		t.Fatalf("removed %d, want 40", removed)
	}
	if err := db.View(func(tx *Tx) error {
		var neighbor string
		found, err := tx.Get("records", "10/neighbor", &neighbor)
		if err != nil || !found || neighbor != "keep" {
			t.Fatalf("neighbor changed: %q %v %v", neighbor, found, err)
		}
		return tx.ScanKeys("records", "1/", "", 1, func(string, int64) error {
			t.Fatal("deleted prefix still has a row")
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupBudgetsCancellationAndTransactionRollback(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *Tx) error {
		for i := range 8 {
			if err := tx.Put("rows", fmt.Sprintf("p/%d", i), string(bytes.Repeat([]byte{'x'}, 256))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := transactionID(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.DeletePrefixPage(ctx, "rows", "p/", "", DefaultCleanupBudget())
		return err
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled deletion: %v", err)
	}
	if transactionID(t, db) != before {
		t.Fatal("canceled cleanup committed")
	}
	for _, test := range []struct {
		name        string
		budget      CleanupBudget
		wantDeleted int64
		wantDone    bool
	}{
		{"records", CleanupBudget{MaxRecords: 1, MaxBytes: 1 << 20, MaxDuration: 25 * time.Millisecond}, 1, false},
		{"bytes", CleanupBudget{MaxRecords: 50, MaxBytes: 1, MaxDuration: 25 * time.Millisecond}, 1, false},
		{"gather_deadline", CleanupBudget{MaxRecords: 50, MaxBytes: 1 << 20, MaxDuration: 25 * time.Millisecond}, 1, false},
		{"delete_deadline", CleanupBudget{MaxRecords: 50, MaxBytes: 1 << 20, MaxDuration: 25 * time.Millisecond}, 1, false},
		{"before_deadline", CleanupBudget{MaxRecords: 50, MaxBytes: 1 << 20, MaxDuration: 25 * time.Millisecond}, 8, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rollback := errors.New("simulated operation cancellation before commit")
			if err := db.Update(func(tx *Tx) error {
				// OS clock precision must not decide which budget stops this page.
				// For delete_deadline, time advances only after the first mutation,
				// so all keys are gathered before the deletion loop must yield.
				base := time.Unix(0, 0)
				clockReads := 0
				checkedBeforeMutation := false
				now := func() time.Time {
					clockReads++
					if clockReads > 1 && !tx.dirty {
						checkedBeforeMutation = true
					}
					if test.name == "gather_deadline" && clockReads > 1 || test.name == "delete_deadline" && tx.dirty {
						return base.Add(test.budget.MaxDuration)
					}
					return base
				}
				page, err := tx.deletePrefixPage(context.Background(), "rows", "p/", "", test.budget, now)
				if err != nil {
					return err
				}
				if page.Deleted != test.wantDeleted || page.Done != test.wantDone || page.After != fmt.Sprintf("p/%d", test.wantDeleted-1) {
					t.Fatalf("budget was not respected: %+v", page)
				}
				// A deletion-loop deadline can also produce one deleted row.
				// Prove collection itself stopped at its first elapsed-time check,
				// before writing anything or gathering further keys.
				if test.name == "gather_deadline" && (!checkedBeforeMutation || clockReads != 2) {
					t.Fatalf("collection ignored its deadline: checked before mutation=%v clock reads=%d", checkedBeforeMutation, clockReads)
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if transactionID(t, db) != before {
				t.Fatal("partial cleanup escaped rollback")
			}
			if err := db.View(func(tx *Tx) error {
				for i := range 8 {
					var value string
					found, err := tx.Get("rows", fmt.Sprintf("p/%d", i), &value)
					if err != nil || !found || value != string(bytes.Repeat([]byte{'x'}, 256)) {
						t.Fatalf("rollback lost row %d: found=%v error=%v", i, found, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
