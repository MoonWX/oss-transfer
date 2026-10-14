package task

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func testSpec() Spec {
	return Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "bucket"}, Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "bucket"}, Nodes: []Node{{ID: "a", EnrollmentID: "ea"}, {ID: "b", EnrollmentID: "eb"}}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", TargetPrefix: "prefix"}
}
func openTest(t *testing.T, path string) (*state.Store, *Repository) {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db, New(db)
}
func objects(k string) []transferstore.Object {
	return []transferstore.Object{{Key: k, Size: 1024, ETag: k}}
}
func finished(c Child) execution.Record {
	return execution.Record{Job: c.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: 1, TotalBytes: 1024, Succeeded: 1, SucceededBytes: 1024}}
}

func TestConcurrentCreateDispatchAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	var group sync.WaitGroup
	ids := make(chan int64, 20)
	errs := make(chan error, 20)
	for range 20 {
		group.Add(1)
		go func() { defer group.Done(); p, _, err := repo.Create(testSpec(), "request"); ids <- p.ID; errs <- err }()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := range ids {
		if id != 1 {
			t.Fatalf("duplicate task %d", id)
		}
	}
	other := testSpec()
	other.TargetPrefix = "other"
	if _, _, err := repo.Create(other, "request"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	first, err := repo.Assign(1, 1, 1, objects("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Assign(1, 1, 2, objects("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.ChildID != "1-1" || second.Job.ChildID != "1-2" || first.Node.ID == second.Node.ID || first.Job.Items[0].DestinationKey != "prefix/one" {
		t.Fatal("bad child identity, mapping or distribution")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	again, err := repo.Assign(1, 1, 1, objects("one"))
	if err != nil || again.Node != first.Node {
		t.Fatal("assignment changed after restart", err)
	}
	if _, err := repo.Assign(1, 1, 1, objects("changed")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	p, err := repo.Get(1)
	if err != nil || p.ChildCount != 2 {
		t.Fatal(p, err)
	}
}
func TestCompletionRequiresEveryDiscoveredBatchAndDeduplicatesProgress(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "req")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ScanProgress(p.ID, 1, 2, 2048, 2, true, ""); err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, objects("one"))
	if err != nil {
		t.Fatal(err)
	}
	report := finished(c)
	for range 3 {
		if err := repo.Observe(p.ID, 1, 1, report); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || done {
		t.Fatal("completed before second batch dispatch", err)
	}
	c, err = repo.Assign(p.ID, 1, 2, objects("two"))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || done {
		t.Fatal("accepted is not completed", err)
	}
	if err := repo.Observe(p.ID, 1, 2, finished(c)); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || !done {
		t.Fatal(done, err)
	}
	p, err = repo.Get(p.ID)
	if err != nil || p.Counters.Succeeded != 2 || p.FinishedChildren != 2 {
		t.Fatal(p, err)
	}
	active, err := repo.List(0, 10, true)
	if err != nil || len(active) != 0 {
		t.Fatal(active, err)
	}
}
func TestStopResumeFencesLateAssignmentsAndCallbacks(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "req")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, objects("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Assign(p.ID, 1, 2, objects("two")); !errors.Is(err, ErrState) {
		t.Fatal("late assignment passed stop", err)
	}
	if err := repo.Stopped(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil || p.Generation != 2 {
		t.Fatal(p, err)
	}
	if err := repo.Observe(p.ID, 1, 1, finished(c)); !errors.Is(err, ErrGeneration) {
		t.Fatal("late old success changed resumed task", err)
	}
	if err := repo.Stopped(p.ID, 1); !errors.Is(err, ErrGeneration) {
		t.Fatal("late stop killed resumed task", err)
	}
	if err := repo.Resumed(p.ID, 2); err != nil {
		t.Fatal(err)
	}
	children, err := repo.Children(p.ID, 0, 1)
	if err != nil || len(children) != 1 || children[0].Node != c.Node || children[0].Job.Generation != 2 {
		t.Fatal(children, err)
	}
}
