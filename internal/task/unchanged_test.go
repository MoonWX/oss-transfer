package task

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

func databaseDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// A bbolt commit changes its on-disk metadata even when the callback writes
// nothing. Comparing the real file therefore detects empty commits as well as
// unwanted application writes; no timer or mocked Store is involved.
func assertNoTaskCommit(t *testing.T, path string, fn func() error) {
	t.Helper()
	before := databaseDigest(t, path)
	if err := fn(); err != nil {
		t.Fatal(err)
	}
	if after := databaseDigest(t, path); after != before {
		t.Fatal("unchanged task observation wrote the database")
	}
}

func TestUnchangedObservationsRollbackWhileChangesRemainDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, _, err := repo.Create(testSpec(), "request")
	if err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, func() error { return repo.RuntimeProblem(p.ID, 1, "") })
	assertNoTaskCommit(t, path, func() error { return repo.ScanProgress(p.ID, 1, 0, 0, 0, false, "") })
	assertNotReady := func() error {
		done, err := repo.FinishIfReady(p.ID, 1)
		if done {
			t.Fatal("unfinished task became complete")
		}
		return err
	}
	assertNoTaskCommit(t, path, assertNotReady)
	if err := repo.RuntimeProblem(p.ID, 1, "Agent unavailable"); err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, func() error { return repo.RuntimeProblem(p.ID, 1, "Agent unavailable") })
	if err := repo.ScanProgress(p.ID, 1, 2, 2048, 2, true, ""); err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, func() error { return repo.ScanProgress(p.ID, 1, 2, 2048, 2, true, "") })
	assertNoTaskCommit(t, path, func() error { return repo.ScanProgress(p.ID, 1, 1, 1024, 1, true, "") })
	assertNoTaskCommit(t, path, assertNotReady) // A discovered batch is not an accepted child.
	first, err := repo.Assign(p.ID, 1, 1, objects("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Assign(p.ID, 1, 2, objects("second"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, assertNotReady) // An accepted child is not finished.
	record := finished(first)
	record.Status = execution.Running
	record.Counters.Succeeded, record.Counters.SucceededBytes = 0, 0
	if err := repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	assertNoTaskCommit(t, path, func() error { return repo.Observe(p.ID, 1, 1, record) })
	older := record
	older.UpdatedAt = older.UpdatedAt.Add(-time.Second)
	assertNoTaskCommit(t, path, func() error { return repo.Observe(p.ID, 1, 1, older) })
	older.Attempt--
	assertNoTaskCommit(t, path, func() error { return repo.Observe(p.ID, 1, 1, older) })
	for _, fn := range []func() error{
		func() error { return repo.RuntimeProblem(p.ID, 2, "Agent unavailable") },
		func() error { return repo.ScanProgress(p.ID, 2, 2, 2048, 2, true, "") },
		func() error { return repo.Observe(p.ID, 2, 1, record) },
		func() error { _, err := repo.FinishIfReady(p.ID, 2); return err },
	} {
		before := databaseDigest(t, path)
		if err := fn(); !errors.Is(err, ErrGeneration) {
			t.Fatalf("stale generation no longer rejected: %v", err)
		}
		if databaseDigest(t, path) != before {
			t.Fatal("rejected generation wrote the database")
		}
	}
	for _, child := range []Child{first, second} {
		if err := repo.Observe(p.ID, 1, child.Job.Number, finished(child)); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || !done {
		t.Fatal("complete task did not commit", done, err)
	}
	assertNoTaskCommit(t, path, assertNotReady) // Repeating completion is also read-only.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	reopened, err := repo.Get(p.ID)
	if err != nil || reopened.Status != "COMPLETED" || reopened.FinishedAt == nil || reopened.Counters.Succeeded != 2 || reopened.ScannedObjects != 2 || reopened.RuntimeError != "Agent unavailable" {
		t.Fatalf("real state changes did not survive reopen: %+v %v", reopened, err)
	}
	children, err := repo.Children(p.ID, 0, 10)
	if err != nil || len(children) != 2 || children[0].Record.Status != execution.Completed || children[1].Record.Status != execution.Completed {
		t.Fatal("durable child ledger missing", err)
	}
}
