package task

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestListingIndexesMigrateOnceAndStayAtomicAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, repo := openTest(t, path)
	// One transaction represents a pre-index Go database, without 10,001 fsyncs.
	if err := db.Update(func(tx *state.Tx) error {
		for i := int64(1); i <= 10001; i++ {
			if _, err := tx.NextID("master-tasks"); err != nil {
				return err
			}
			p := Parent{ID: i, Status: "COMPLETED", Generation: 1, Spec: testSpec()}
			if err := tx.Put("master-tasks", key(i), p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureListingIndex(); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Recent(0, 10000, false)
	if err != nil || len(latest) != 10000 || latest[0].ID != 10001 || latest[9999].ID != 2 {
		t.Fatal("latest boundary lost new tasks", len(latest), err)
	}
	older, err := repo.Recent(2, 10, false)
	if err != nil || len(older) != 1 || older[0].ID != 1 {
		t.Fatal("exclusive cursor skipped oldest task", older, err)
	}
	if _, err := repo.Recycle(10001, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Recycle(10000, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Purge(10000); err != nil {
		t.Fatal(err)
	}
	bin, err := repo.Recent(0, 10, true)
	if err != nil || len(bin) != 1 || bin[0].ID != 10001 {
		t.Fatal("recycle index contains purged task", bin, err)
	}
	if _, err := repo.Recycle(10001, false); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort transaction")
	if err := db.Update(func(tx *state.Tx) error {
		p, err := get(tx, 9999)
		if err != nil {
			return err
		}
		p.Deleted = true
		if err := save(tx, p); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	if err := repo.EnsureListingIndex(); err != nil {
		t.Fatal(err)
	}
	latest, err = repo.Recent(0, 3, false)
	if err != nil || len(latest) != 3 || latest[0].ID != 10001 || latest[1].ID != 9999 || latest[2].ID != 9998 {
		t.Fatal("index lost rollback/restore/purge across restart", latest, err)
	}
	bin, err = repo.Recent(0, 10, true)
	if err != nil || len(bin) != 0 {
		t.Fatal("recycle retained restored or rolled-back entries", bin, err)
	}
}

func TestListingMigrationAndConcurrentCreatesDoNotLoseRows(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	var group sync.WaitGroup
	errs := make(chan error, 9)
	group.Add(1)
	go func() { defer group.Done(); errs <- repo.EnsureListingIndex() }()
	for i := range 8 {
		group.Add(1)
		go func() { defer group.Done(); _, _, err := repo.Create(testSpec(), fmt.Sprint(i)); errs <- err }()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repo.Recent(0, 20, false)
	if err != nil || len(rows) != 8 {
		t.Fatal("concurrent create lost indexed task", len(rows), err)
	}
	for i, row := range rows {
		if row.ID != int64(8-i) {
			t.Fatal("duplicate or missing ID", rows)
		}
	}
}
