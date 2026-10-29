package task

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestBackgroundPurgeAdmissionRestartFencesAndNeighborIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	p, _, err := repo.Create(testSpec(), "delete-me")
	if err != nil {
		t.Fatal(err)
	}
	keep, _, err := repo.Create(testSpec(), "keep-me")
	if err != nil {
		t.Fatal(err)
	}
	const count = 6000
	if err := db.Update(func(tx *state.Tx) error {
		p.Status, p.ChildCount = "COMPLETED", count
		keep.Status, keep.ChildCount = "PARTIAL_FAILED", 1
		for i := 1; i <= count; i++ {
			for _, bucket := range []string{"master-task-children", "master-task-outstanding", allChildOrderBucket} {
				if err := tx.Put(bucket, childKey(p.ID, i), map[string]any{"parentId": p.ID, "number": i, "object": fmt.Sprintf("training/data/%010d", i)}); err != nil {
					return err
				}
			}
		}
		if err := tx.Put("master-task-children", childKey(keep.ID, 1), "retain this ledger"); err != nil {
			return err
		}
		for _, parent := range []Parent{p, keep} {
			for _, entry := range [][2]string{
				{"master-task-leases", key(parent.ID) + "/a"},
				{"master-task-storage-proofs", fmt.Sprint(parent.ID)},
				{"master-task-legacy-original", key(parent.ID)},
			} {
				if err := tx.Put(entry[0], entry[1], "auxiliary record"); err != nil {
					return err
				}
			}
		}
		if err := tx.Put("master-task-storage-proofs", "10", "prefix neighbor"); err != nil {
			return err
		}
		if err := repo.save(tx, p); err != nil {
			return err
		}
		return repo.save(tx, keep)
	}); err != nil {
		t.Fatal(err)
	}
	retained, err := repo.Get(keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.RequestPurge(p.ID); !errors.Is(err, ErrState) {
		t.Fatalf("purged a visible task: %v", err)
	}
	if _, err := repo.Recycle(p.ID, true); err != nil {
		t.Fatal(err)
	}
	p, admitted, err := repo.RequestPurge(p.ID)
	if err != nil || !admitted || p.Purged || p.Cleanup == nil || p.Cleanup.Status != "QUEUED" || p.Cleanup.TotalNodes != 2 {
		t.Fatalf("bad durable admission: %+v %v %v", p, admitted, err)
	}
	assertNoTaskCommit(t, path, func() error {
		_, admitted, err := repo.RequestPurge(p.ID)
		if admitted {
			t.Fatal("duplicate admission accepted again")
		}
		return err
	})
	for _, fn := range []func() error{
		func() error { _, err := repo.Recycle(p.ID, false); return err },
		func() error { _, err := repo.Resume(p.ID); return err },
		func() error { return repo.Observe(p.ID, p.Generation, 1, finished(Child{})) },
		func() error { _, _, err := repo.PurgePage(context.Background(), p.ID); return err },
		func() error {
			_, err := repo.UpdatePurge(p.ID, func(c *CleanupProgress) error { c.Phase = "MASTER"; return nil })
			return err
		},
	} {
		if err := fn(); !errors.Is(err, ErrState) {
			t.Fatalf("cleanup fence did not reject operation: %v", err)
		}
	}
	if _, err := repo.UpdatePurge(p.ID, func(c *CleanupProgress) error {
		c.Nodes["a"] = CleanupNodeProgress{Done: true, RecordsDeleted: 10}
		c.Nodes["b"] = CleanupNodeProgress{Skipped: true, Error: "enrollment revoked"}
		c.Phase, c.Status = "MASTER", "RUNNING"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, done, err := repo.PurgePage(context.Background(), p.ID)
	if err != nil || done || p.Purged || p.Cleanup.LocalRecordsDeleted < 1 || p.Cleanup.LocalRecordsDeleted > 2048 {
		t.Fatalf("first page not bounded: %+v %v %v", p.Cleanup, done, err)
	}
	afterFirstPage := *p.Cleanup
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	pending, err := repo.PendingPurges(0, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != p.ID || !reflect.DeepEqual(*pending[0].Cleanup, afterFirstPage) {
		t.Fatalf("restart lost cursor: %+v %v", pending, err)
	}
	var pages int
	for {
		pages++
		p, done, err = repo.PurgePage(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		// An unrelated task write is admitted between cleanup pages.
		if err := repo.RuntimeProblem(keep.ID, keep.Generation, fmt.Sprintf("page %d", pages)); err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if pages > 100 {
			t.Fatal("cleanup failed to converge")
		}
	}
	if !p.Purged || p.Cleanup.Status != "DONE" || p.Cleanup.LocalRecordsDeleted != count*3+3 || p.Cleanup.RecordsDeleted != count*3+3+10 {
		t.Fatalf("bad completed progress: %+v", p.Cleanup)
	}
	pending, err = repo.PendingPurges(0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed operation still queued: %+v %v", pending, err)
	}
	if err := repo.RuntimeProblem(keep.ID, keep.Generation, retained.RuntimeError); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Get(keep.ID)
	if err != nil || !reflect.DeepEqual(retained, after) {
		t.Fatalf("neighbor changed: %+v %+v %v", retained, after, err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var value string
		ok, err := tx.Get("master-task-children", childKey(keep.ID, 1), &value)
		if err != nil || !ok || value != "retain this ledger" {
			t.Fatalf("neighbor ledger changed: %q %v %v", value, ok, err)
		}
		for _, bucket := range cleanupBuckets {
			if err := tx.ScanKeys(bucket, key(p.ID)+"/", "", 1, func(string, int64) error {
				t.Fatalf("purged record remains in %s", bucket)
				return nil
			}); err != nil {
				return err
			}
		}
		for _, entry := range [][3]string{
			{"master-task-leases", key(p.ID) + "/a", key(keep.ID) + "/a"},
			{"master-task-storage-proofs", fmt.Sprint(p.ID), fmt.Sprint(keep.ID)},
			{"master-task-legacy-original", key(p.ID), key(keep.ID)},
		} {
			var record string
			found, err := tx.Get(entry[0], entry[1], &record)
			if err != nil || found {
				t.Fatalf("cleanup retained auxiliary record %s/%s: %v", entry[0], entry[1], err)
			}
			found, err = tx.Get(entry[0], entry[2], &record)
			if err != nil || !found || record != "auxiliary record" {
				t.Fatalf("neighbor auxiliary record changed %s/%s: %v", entry[0], entry[2], err)
			}
		}
		var proof string
		found, err := tx.Get("master-task-storage-proofs", "10", &proof)
		if err != nil || !found || proof != "prefix neighbor" {
			t.Fatalf("task 1 cleanup affected task 10 proof: %q %v %v", proof, found, err)
		}
		var format struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		_, err = tx.Get("metadata", "format", &format)
		if format.SchemaVersion != state.SchemaBackgroundCleanup {
			t.Fatalf("old writers not fenced: %+v", format)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, func() error {
		_, admitted, err := repo.RequestPurge(p.ID)
		if admitted {
			t.Fatal("completed cleanup admitted again")
		}
		return err
	})
}
