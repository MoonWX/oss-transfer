package task

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// heavyParent resembles a long-running multi-node task: 16 nodes, a scheduling
// and failover map per node, a scan checkpoint and child-order state.
func heavyParent(id int64, status string) Parent {
	spec := testSpec()
	spec.Nodes = nil
	scheduling := &SchedulingState{Nodes: map[string]PendingLoad{}}
	failover := &FailoverState{Enabled: true, NodeEpochs: map[string]uint64{}, RebalancedEpochs: map[string]uint64{}, Rejoining: map[string]uint64{}, Migrations: map[string]FailoverMigration{}, Retired: map[string]RetiredNode{}}
	for n := range 16 {
		node := fmt.Sprintf("node-%02d-%s", n, strings.Repeat("x", 24))
		spec.Nodes = append(spec.Nodes, Node{ID: node, EnrollmentID: "enrollment-" + node, Address: fmt.Sprintf("10.0.%d.%d:9000", n, id%250), LocalRoot: "/data/transfer/" + node})
		scheduling.Nodes[node] = PendingLoad{Bytes: int64(n) << 30, Objects: int64(n) * 1000, Children: int64(n)}
		failover.NodeEpochs[node], failover.RebalancedEpochs[node], failover.Rejoining[node] = uint64(n), uint64(n), uint64(n)
		failover.Migrations[node] = FailoverMigration{After: n, Through: n * 2, StartedAt: time.Unix(1700000000, 0).UTC()}
	}
	failover.Retired[spec.Nodes[15].ID] = RetiredNode{RetiredAt: time.Unix(1700000000, 0).UTC()}
	created := time.Unix(1700000000+id, 0).UTC()
	finished := created.Add(time.Hour)
	resumeAfter := 7
	return Parent{
		ID: id, Spec: spec, Generation: 3, BudgetRevision: 9, Status: status, CreatedAt: created, StartedAt: &created, FinishedAt: &finished,
		ActiveTime:     &execution.ActiveTime{ElapsedMillis: 3600000},
		ScanCheckpoint: &discovery.RecoveryCheckpoint{Sequence: 99, Version: 1, ThroughBatch: 4000, Objects: 512000, Bytes: 1 << 40, Cursor: strings.Repeat("prefix/key/", 40)},
		ResumeAfter:    &resumeAfter, ScannedObjects: 512000, ScannedBytes: 1 << 40, ScanComplete: true, ScannedBatches: 4000, DispatchedThrough: 4000,
		DispatchedObjects: 512000, DispatchedBytes: 1 << 40, ChildCount: 4000, FinishedChildren: 4000,
		Counters:    execution.Counters{Total: 512000, TotalBytes: 1 << 40, Succeeded: 511000, SucceededBytes: 1<<40 - 1000, Failed: 1000, FailedBytes: 1000},
		Fingerprint: strings.Repeat("f", 64), Scheduling: scheduling, Failover: failover,
		ChildOrder: &ChildOrderState{Version: 1, All: &AllChildOrderState{Version: 1, Revision: 4000, Ready: true}},
		Cleanup:    &CleanupProgress{Status: "RUNNING", Phase: "agents", TotalNodes: 16, Nodes: map[string]CleanupNodeProgress{spec.Nodes[0].ID: {}}, After: strings.Repeat("k", 100)},
	}
}

func summaryRow(t testing.TB, db *state.Store, id int64) (Parent, bool) {
	t.Helper()
	var p Parent
	var found bool
	if err := db.View(func(tx *state.Tx) error {
		var err error
		found, err = tx.Get(summaryBucket, key(id), &p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p, found
}

func TestListingSummaryIsWrittenWithEverySaveAndDropsRuntimeState(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	created, _, err := repo.Create(testSpec(), "summary")
	if err != nil {
		t.Fatal(err)
	}
	full := heavyParent(created.ID, "COMPLETED")
	if err := db.Update(func(tx *state.Tx) error { return save(tx, full) }); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary, found := summaryRow(t, db, created.ID)
	if !found || !reflect.DeepEqual(summary, listingSummary(stored)) {
		t.Fatalf("summary not written with the parent\n%#v\n%#v", summary, listingSummary(stored))
	}
	if summary.ScanCheckpoint != nil || summary.Scheduling != nil || summary.ChildOrder != nil || summary.ResumeAfter != nil || summary.Fingerprint != "" || len(summary.Failover.NodeEpochs) != 0 || len(summary.Failover.Retired) != 1 || summary.Cleanup.Nodes != nil {
		t.Fatalf("summary kept runtime state: %#v", summary)
	}
	if len(summary.ActiveNodes()) != 15 || summary.ExecutionTime() != stored.ExecutionTime() {
		t.Fatal("summary changed derived list fields")
	}
	fullJSON, _ := json.Marshal(stored)
	summaryJSON, _ := json.Marshal(summary)
	if len(summaryJSON)*2 > len(fullJSON) {
		t.Fatalf("summary is not compact: %d vs %d bytes", len(summaryJSON), len(fullJSON))
	}
	// Repeated polls reuse decoded rows until the stored summary changes.
	for range 3 {
		if rows, err := repo.RecentSummaries(0, 10, false); err != nil || len(rows) != 1 || rows[0].Counters != stored.Counters {
			t.Fatal(rows, err)
		}
	}
	if decodes := repo.summaries.decodes.Load(); decodes != 1 {
		t.Fatal("unchanged summary decoded again", decodes)
	}
	stored.Counters.Succeeded++
	if err := db.Update(func(tx *state.Tx) error { return save(tx, stored) }); err != nil {
		t.Fatal(err)
	}
	if rows, err := repo.RecentSummaries(0, 10, false); err != nil || len(rows) != 1 || rows[0].Counters != stored.Counters || repo.summaries.decodes.Load() != 2 {
		t.Fatal("changed summary served from cache", rows, err)
	}
	// Purged parents leave the listing, and their summary goes with them.
	stored.Cleanup = nil
	if err := db.Update(func(tx *state.Tx) error { return save(tx, stored) }); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Recycle(created.ID, true); err != nil {
		t.Fatal(err)
	}
	if bin, err := repo.RecentSummaries(0, 10, true); err != nil || len(bin) != 1 || !bin[0].Deleted {
		t.Fatal("recycle page lost summary", bin, err)
	}
	if _, err := repo.Purge(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, found := summaryRow(t, db, created.ID); found {
		t.Fatal("purged parent kept a summary")
	}
}

func TestEnsureListingSummariesRepairsRowsWrittenByOlderMasters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, repo := openTest(t, path)
	var ids []int64
	for i := range 5 {
		p, _, err := repo.Create(testSpec(), fmt.Sprint("summary-", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if err := db.Update(func(tx *state.Tx) error {
		p, err := get(tx, ids[4])
		if err != nil {
			return err
		}
		p.Status, p.Deleted = "STOPPED", true
		return save(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	// An older Master rewrites parents without touching summaries: one row
	// becomes stale, one is missing (a task created by the older binary), and
	// one is orphaned because the older binary purged its parent.
	if err := db.Update(func(tx *state.Tx) error {
		stale, err := get(tx, ids[0])
		if err != nil {
			return err
		}
		stale.Status, stale.Counters.Succeeded = "COMPLETED", 42
		if err := tx.Put("master-tasks", key(stale.ID), stale); err != nil {
			return err
		}
		if err := tx.Delete(summaryBucket, key(ids[1])); err != nil {
			return err
		}
		purged, err := get(tx, ids[2])
		if err != nil {
			return err
		}
		purged.Deleted, purged.Purged = true, true
		if err := tx.Put("master-tasks", key(purged.ID), purged); err != nil {
			return err
		}
		return syncListingIndex(tx, purged)
	}); err != nil {
		t.Fatal(err)
	}
	if rows, err := repo.RecentSummaries(0, 10, false); err != nil || len(rows) != 3 || rows[2].Status == "COMPLETED" || rows[1].ID != ids[1] {
		t.Fatal("fixture did not model a stale summary", rows, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	if err := repo.EnsureListingSummaries(); err != nil {
		t.Fatal(err)
	}
	for _, deleted := range []bool{false, true} {
		full, err := repo.Recent(0, 10, deleted)
		if err != nil {
			t.Fatal(err)
		}
		summaries, err := repo.RecentSummaries(0, 10, deleted)
		if err != nil || len(summaries) != len(full) {
			t.Fatal(len(summaries), len(full), err)
		}
		for i := range full {
			if !reflect.DeepEqual(summaries[i], listingSummary(full[i])) {
				t.Fatalf("rebuild left a stale row\n%#v\n%#v", summaries[i], listingSummary(full[i]))
			}
		}
	}
	if _, found := summaryRow(t, db, ids[2]); found {
		t.Fatal("rebuild kept an orphaned summary")
	}
	// A repeated start is idempotent.
	if err := repo.EnsureListingSummaries(); err != nil {
		t.Fatal(err)
	}
	if rows, err := repo.RecentSummaries(0, 10, false); err != nil || len(rows) != 3 || rows[2].Status != "COMPLETED" || rows[2].Counters.Succeeded != 42 {
		t.Fatal("repeated rebuild changed rows", rows, err)
	}
}

// BenchmarkRecentListing measures one console status poll's repository read
// with N historical tasks: full parents versus compact summaries.
//
//	go test ./internal/task -run '^$' -bench RecentListing -benchmem
func BenchmarkRecentListing(b *testing.B) {
	for _, total := range []int{1000, 10000} {
		db, repo := openTest(b, filepath.Join(b.TempDir(), "db"))
		if err := db.Update(func(tx *state.Tx) error {
			for i := int64(1); i <= int64(total); i++ {
				if _, err := tx.NextID("master-tasks"); err != nil {
					return err
				}
				if err := save(tx, heavyParent(i, "COMPLETED")); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		for _, mode := range []string{"full", "summary"} {
			read := repo.Recent
			if mode == "summary" {
				read = repo.RecentSummaries
			}
			rows, err := read(0, 10001, false)
			if err != nil || len(rows) != total {
				b.Fatal(len(rows), err)
			}
			// JSON bytes a decode of every row would parse.
			jsonBytes := 0
			for _, row := range rows {
				data, _ := json.Marshal(row)
				jsonBytes += len(data)
			}
			b.Run(fmt.Sprintf("tasks=%d/%s", total, mode), func(b *testing.B) {
				b.ReportAllocs()
				before := repo.summaries.decodes.Load()
				for range b.N {
					if _, err := read(0, 10001, false); err != nil {
						b.Fatal(err)
					}
				}
				decoded := float64(total)
				if mode == "summary" {
					// Rows decoded per poll once the cache is warm (the first
					// poll above warmed it).
					decoded = float64(repo.summaries.decodes.Load()-before) / float64(b.N)
				}
				b.ReportMetric(decoded, "recordsDecoded/op")
				b.ReportMetric(decoded*float64(jsonBytes)/float64(total)/1024, "decodedKiB/op")
			})
		}
		db.Close()
	}
}
