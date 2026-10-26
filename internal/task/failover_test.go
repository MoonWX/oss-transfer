package task

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func failoverRecord(child Child, completed bool) execution.Record {
	now := time.Now().UTC()
	r := execution.Record{Job: child.Job, Status: execution.PartialFailed, Attempt: 7, UpdatedAt: now, CreatedAt: now.Add(-time.Minute), StartedAt: new(now.Add(-time.Minute)), FinishedAt: &now}
	r.Counters = execution.Counters{Total: 3, TotalBytes: 300, Succeeded: 1, SucceededBytes: 100, Skipped: 1, SkippedBytes: 100, Failed: 1, FailedBytes: 100}
	for index, item := range child.Job.Items {
		result := execution.ItemResult{ID: item.ID, Status: execution.ItemSucceeded, Target: transferstore.Object{Key: item.DestinationKey, Size: 100, ETag: "target-version"}}
		switch index {
		case 1:
			result.Status = execution.ItemSkipped
		case 2:
			result.Status, result.Error = execution.ItemFailed, "upload failed"
		}
		r.Results = append(r.Results, result)
	}
	if completed {
		r.Status = execution.Completed
		r.Results[2].Status, r.Results[2].Error = execution.ItemSucceeded, ""
		r.Counters.Succeeded, r.Counters.SucceededBytes = 2, 200
		r.Counters.Failed, r.Counters.FailedBytes = 0, 0
	}
	return r
}

func createFailoverTask(t *testing.T, repo *Repository, spec Spec, children int) (Parent, []Child) {
	t.Helper()
	p, _, err := repo.Create(spec, "failover")
	if err != nil {
		t.Fatal(err)
	}
	var result []Child
	for number := 1; number <= children; number++ {
		rows, err := repo.AssignPageAvailable(p.ID, p.Generation, []Assignment{{Number: number, Objects: schedulingObjects(fmt.Sprint(number), 3, 100)}}, map[string]bool{spec.Nodes[0].ID: true})
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, rows...)
	}
	return p, result
}

func enableFailoverTest(t *testing.T, db *state.Store, repo *Repository, p Parent, shared bool) Parent {
	t.Helper()
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	current, err := repo.EnableFailover(p.ID, p.Generation, shared)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestFailoverBatchesPreserveResultsCountersAndResumeAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	p, children := createFailoverTask(t, repo, testSpec(), 260)
	var observations []Observation
	for index, child := range children {
		observations = append(observations, Observation{Number: child.Job.Number, Record: failoverRecord(child, index == 0)})
	}
	for offset := 0; offset < len(observations); offset += 128 {
		if err := repo.ObserveBatch(p.ID, p.Generation, observations[offset:min(offset+128, len(observations))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ScanProgress(p.ID, p.Generation, 780, 78000, 260, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, ready, err := repo.PriorityChildren(context.Background(), p.ID, "", 5, nil); err != nil || !ready {
		t.Fatal("priority index", ready, err)
	}
	before, _ := repo.Get(p.ID)
	p = enableFailoverTest(t, db, repo, p, false)
	p, err := repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil || !p.NodeRetired("a") || p.Generation != before.Generation || p.BudgetRevision != before.BudgetRevision+1 {
		t.Fatal("retirement did not retain generation and advance budget", p, err)
	}
	if done, err := repo.FinishIfReady(p.ID, p.Generation); err != nil || done {
		t.Fatal("task ended while migration was pending", done, err)
	}
	first, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true})
	if err != nil || ready || len(first) != 127 {
		t.Fatal("first page was not bounded or moved completed child", len(first), ready, err)
	}
	progress, _ := repo.Get(p.ID)
	if progress.Failover.Migrations["a"].After != 128 || progress.Counters.Succeeded != before.Counters.Succeeded || progress.Counters.Skipped != before.Counters.Skipped || progress.Counters.Failed != 132 {
		t.Fatal("first page counters/cursor", progress)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	if again, err := repo.BeginFailover(p.ID, p.Generation, "a"); err != nil || again.Failover.Migrations["a"].After != 128 {
		t.Fatal("repeated retirement reset cursor", again, err)
	}
	second, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true})
	if err != nil || ready || len(second) != 128 || second[0].Job.Number != 129 {
		t.Fatal("restart replayed or skipped assignments", len(second), ready, err)
	}
	last, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true})
	if err != nil || !ready || len(last) != 4 || last[0].Job.Number != 257 {
		t.Fatal("final page", len(last), ready, err)
	}
	if rows, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", nil); err != nil || !ready || len(rows) != 0 {
		t.Fatal("completed migration was not idempotent", ready, err)
	}
	current, _ := repo.Get(p.ID)
	if current.ChildCount != 260 || current.DispatchedThrough != 260 || current.Generation != 1 || current.FinishedChildren != 1 || current.FailedChildren != 0 || current.Counters.Failed != 0 ||
		current.Counters.Succeeded != before.Counters.Succeeded || current.Counters.Skipped != before.Counters.Skipped || current.Counters.Total != before.Counters.Total ||
		current.Scheduling.Nodes["a"] != (PendingLoad{}) || current.Scheduling.Nodes["b"] != (PendingLoad{Children: 259, Objects: 259, Bytes: 25900}) {
		t.Fatal("final counters or node loads", current)
	}
	remaining, err := repo.Children(p.ID, 0, 256)
	if err != nil || remaining[0].Node.ID != "a" || !reflect.DeepEqual(*remaining[0].Record, observations[0].Record) {
		t.Fatal("completed record was modified", err)
	}
	for _, child := range append(append(first, second...), last...) {
		if child.Node.ID != "b" || child.Job.Assignment != 1 || child.Accepted || child.Record == nil || child.Record.Job.Assignment != 1 || child.Record.Status != execution.Recoverable || child.Record.Attempt != 7 || child.Record.FinishedAt != nil || child.Record.Results[0].Status != execution.ItemSucceeded || child.Record.Results[1].Status != execution.ItemSkipped || child.Record.Results[2].Status != execution.ItemPending {
			t.Fatal("seed lost object results or ownership", child)
		}
	}
	if err := repo.Observe(p.ID, p.Generation, 2, observations[1].Record); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted late old-owner result", err)
	}
	var exceptionCount int
	if err := repo.VisitExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}, func(Exception) error { exceptionCount++; return nil }); err != nil || exceptionCount != 0 {
		t.Fatal("old failures remained indexed", exceptionCount, err)
	}
	active, _, err := repo.ActiveChildren(p.ID, "", 256, nil)
	all, _, ready, allErr := repo.PriorityChildren(context.Background(), p.ID, "", 256, nil)
	if err != nil || allErr != nil || !ready || len(active) != 256 || len(all) != 256 || active[0].Job.Number != 2 || all[0].Job.Number != 2 {
		t.Fatal("status order indexes were not updated", err, allErr)
	}
	outstanding, err := repo.Outstanding(p.ID, 0, 256)
	if err != nil || len(outstanding) != 256 || outstanding[0].Job.Number != 2 {
		t.Fatal("outstanding index", len(outstanding), err)
	}
	newChild, err := repo.Assign(p.ID, p.Generation, 261, objects("new"))
	if err != nil || newChild.Node.ID != "b" {
		t.Fatal("retired node received new assignment", newChild, err)
	}
}

func TestFailoverRejectsUnsafeTargetsAndPreservesCursorOnError(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, _ := createFailoverTask(t, repo, testSpec(), 2)
	if _, err := repo.BeginFailover(p.ID, p.Generation, "a"); !errors.Is(err, ErrState) {
		t.Fatal("failover started without capability checks", err)
	}
	p = enableFailoverTest(t, db, repo, p, false)
	if _, err := repo.BeginFailover(p.ID, p.Generation, "outsider"); !errors.Is(err, ErrFailoverTarget) {
		t.Fatal("unknown node retired", err)
	}
	p, err := repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, available := range []map[string]bool{nil, {}, {"a": true}, {"outsider": true}} {
		if _, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", available); !errors.Is(err, ErrFailoverTarget) || ready {
			t.Fatal("unverified or retired target admitted", ready, err)
		}
		current, _ := repo.Get(p.ID)
		rows, _ := repo.Children(p.ID, 0, 2)
		if current.Failover.Migrations["a"].After != 0 || rows[0].Node.ID != "a" || rows[0].Job.Assignment != 0 {
			t.Fatal("failed page changed durable ownership", current, rows)
		}
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); !errors.Is(err, ErrState) {
		t.Fatal("stopped parent migrated", err)
	}
}

func TestFailoverScannerCheckpointIsBoundedAndEpochFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, repo := openTest(t, path)
	defer func() { _ = db.Close() }()
	spec := testSpec()
	spec.Nodes = append(spec.Nodes, Node{ID: "c", EnrollmentID: "ec"})
	p, _ := createFailoverTask(t, repo, spec, 3)
	p = enableFailoverTest(t, db, repo, p, false)
	if err := repo.ScanProgress(p.ID, p.Generation, 3000, 300000, 1000, true, "old scanner failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BeginFailover(p.ID, p.Generation, "a"); err != nil {
		t.Fatal(err)
	}
	seeding, err := repo.BeginScannerFailover(p.ID, p.Generation, "b")
	if err != nil || seeding.ScannerNode().ID != "b" || seeding.ScannerEpoch() != 1 || !seeding.Failover.ScannerSeeding || seeding.Failover.ScannerThrough != 3 || seeding.ScannedObjects != 9 || seeding.ScannedBytes != 900 || seeding.ScannedBatches != 3 || seeding.ScanComplete || seeding.Error != "" {
		t.Fatal("scanner replacement retained undispatched progress", seeding, err)
	}
	if _, err := repo.FinishScannerSeed(p.ID, p.Generation, 1); !errors.Is(err, ErrState) {
		t.Fatal("incomplete seed finished", err)
	}
	if _, err := repo.AdvanceScannerSeed(p.ID, p.Generation, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	seeding, err = repo.BeginScannerFailover(p.ID, p.Generation, "b")
	if err != nil || seeding.Failover.ScannerSeedAfter != 2 {
		t.Fatal("restart lost scanner seed cursor", seeding, err)
	}
	if _, err := repo.BeginFailover(p.ID, p.Generation, "b"); err != nil {
		t.Fatal(err)
	}
	seeding, err = repo.BeginScannerFailover(p.ID, p.Generation, "c")
	if err != nil || seeding.ScannerEpoch() != 2 || seeding.Failover.ScannerSeedAfter != 0 {
		t.Fatal("second scanner failover", seeding, err)
	}
	if _, err := repo.AdvanceScannerSeed(p.ID, p.Generation, 1, 3); !errors.Is(err, ErrGeneration) {
		t.Fatal("stale scanner acknowledgement accepted", err)
	}
	if _, err := repo.AdvanceScannerSeed(p.ID, p.Generation, 2, 4); !errors.Is(err, ErrState) {
		t.Fatal("seed cursor escaped fixed upper bound", err)
	}
	if _, err := repo.AdvanceScannerSeed(p.ID, p.Generation, 2, 3); err != nil {
		t.Fatal(err)
	}
	seeding, err = repo.FinishScannerSeed(p.ID, p.Generation, 2)
	if err != nil || seeding.Failover.ScannerSeeding {
		t.Fatal("scanner seed not finished", seeding, err)
	}
	if _, err := repo.FinishScannerSeed(p.ID, p.Generation, 2); err != nil {
		t.Fatal("finish seed was not idempotent", err)
	}
	batch := []Assignment{{Number: 4, Objects: objects("new-scanner-object")}}
	if _, err := repo.AssignPage(p.ID, p.Generation, batch); !errors.Is(err, ErrGeneration) {
		t.Fatal("legacy scanner assigned after epoch change", err)
	}
	if _, err := repo.AssignPageForScanner(p.ID, p.Generation, 1, batch, map[string]bool{"c": true}); !errors.Is(err, ErrGeneration) {
		t.Fatal("stale scanner assigned after epoch change", err)
	}
	if err := repo.ScanProgress(p.ID, p.Generation, 9000, 900000, 3000, true, ""); !errors.Is(err, ErrGeneration) {
		t.Fatal("legacy progress restored abandoned scan counts", err)
	}
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 1, 9000, 900000, 3000, true, ""); !errors.Is(err, ErrGeneration) {
		t.Fatal("stale progress restored abandoned scan counts", err)
	}
	rows, err := repo.AssignPageForScanner(p.ID, p.Generation, 2, batch, map[string]bool{"c": true})
	if err != nil || len(rows) != 1 || rows[0].Node.ID != "c" {
		t.Fatal("replacement scanner could not assign", rows, err)
	}
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 2, 10, 1924, 4, true, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFailoverRollsBackWholePageAndContinuesAfterParentResume(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	p, children := createFailoverTask(t, repo, testSpec(), 2)
	good := failoverRecord(children[0], false)
	bad := failoverRecord(children[1], false)
	bad.Results = nil
	if err := repo.ObserveBatch(p.ID, p.Generation, []Observation{{Number: 1, Record: good}, {Number: 2, Record: bad}}); err != nil {
		t.Fatal(err)
	}
	p = enableFailoverTest(t, db, repo, p, false)
	p, err := repo.BeginFailover(p.ID, p.Generation, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown success ledger was discarded", err)
	}
	current, _ := repo.Get(p.ID)
	rows, _ := repo.Children(p.ID, 0, 2)
	if current.Failover.Migrations["a"].After != 0 || rows[0].Node.ID != "a" || rows[0].Job.Assignment != 0 || !reflect.DeepEqual(current.Counters, p.Counters) {
		t.Fatal("failed second child committed the first migration", current, rows)
	}
	bad = failoverRecord(children[1], false)
	if err := repo.Observe(p.ID, p.Generation, 2, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); !errors.Is(err, ErrState) {
		t.Fatal("migration raced unfinished resume preparation", err)
	}
	prepareResume(t, repo, p)
	moved, ready, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true})
	if err != nil || !ready || len(moved) != 2 || moved[0].Job.Generation != p.Generation || moved[0].Record.Counters.Succeeded != 1 {
		t.Fatal("resuming parent could not finish existing migration", moved, ready, err)
	}
}

func TestFailoverSharedPathsPinsAndRepeatedMigration(t *testing.T) {
	for _, direction := range []string{"LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
		t.Run(direction, func(t *testing.T) {
			db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
			defer db.Close()
			spec := testSpec()
			spec.Direction, spec.Nodes[0].LocalRoot, spec.Nodes[1].LocalRoot = direction, "/original", "/replacement"
			spec.Nodes = append(spec.Nodes, Node{ID: "c", EnrollmentID: "ec", LocalRoot: "/third"})
			if direction == "LOCAL_TO_OSS" {
				spec.Source = execution.Endpoint{Kind: "LOCAL", Root: "/original"}
				spec.SharedLocalSource = true
			} else {
				spec.Destination = execution.Endpoint{Kind: "LOCAL", Root: "/original"}
			}
			p, _ := createFailoverTask(t, repo, spec, 1)
			p = enableFailoverTest(t, db, repo, p, direction == "OSS_TO_LOCAL")
			for index, node := range []string{"a", "b"} {
				target := []string{"b", "c"}[index]
				if _, err := repo.BeginFailover(p.ID, p.Generation, node); err != nil {
					t.Fatal(err)
				}
				moved, ready, err := repo.PrepareFailover(p.ID, p.Generation, node, map[string]bool{target: true})
				if err != nil || !ready || len(moved) != 1 || moved[0].Job.Assignment != uint64(index+1) || moved[0].Node.ID != target {
					t.Fatal("repeated node replacement", moved, ready, err)
				}
				job := moved[0].Job
				if direction == "LOCAL_TO_OSS" && job.Source.Root != moved[0].Node.LocalRoot || direction == "OSS_TO_LOCAL" && (job.Destination.Root != moved[0].Node.LocalRoot || !job.SharedLocalDestination) {
					t.Fatal("replacement mount path or proof flag missing", job)
				}
			}
		})
	}
	item := execution.Item{ID: "item", Source: transferstore.Object{Key: "missing"}, DestinationKey: "out"}
	pin := transferstore.Object{Key: "missing", Size: 123, ETag: "first-version"}
	job := execution.Job{Items: []execution.Item{item}}
	old := execution.Record{Job: job, Results: []execution.ItemResult{{ID: "item", Status: execution.ItemFailed, ResolvedSource: &pin}}, Counters: execution.Counters{Total: 1, TotalBytes: 123, Failed: 1, FailedBytes: 123}}
	seed, err := failoverSeed(job, old, time.Now())
	if err != nil || seed.Results[0].ResolvedSource == nil || !reflect.DeepEqual(*seed.Results[0].ResolvedSource, pin) || seed.Counters.TotalBytes != 123 || seed.Counters.Failed != 0 {
		t.Fatal("resolved source identity lost", seed, err)
	}
	old.Counters.Succeeded = 1
	if _, err := failoverSeed(job, old, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatal("inconsistent successful ledger silently discarded", err)
	}
}
