package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	bolt "go.etcd.io/bbolt"
)

// Read a copy of a quiescent database through bbolt's public API. Transaction IDs
// count actual durable commits, independent of Store implementation or fsync speed.
func durableTransactionID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(copyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(copyPath, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int
	if err := db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	return id
}

func observationPage(tb testing.TB, repo *Repository, count, objectsPerChild int) (Parent, []Observation) {
	tb.Helper()
	p, _, err := repo.Create(testSpec(), "observation-page")
	if err != nil {
		tb.Fatal(err)
	}
	observations := make([]Observation, count)
	for i := range observations {
		objects := make([]transferstore.Object, objectsPerChild)
		for j := range objects {
			objects[j] = transferstore.Object{Key: fmt.Sprintf("child-%d/object-%d", i, j), Size: 1024, ETag: "version"}
		}
		child, err := repo.Assign(p.ID, p.Generation, i+1, objects)
		if err != nil {
			tb.Fatal(err)
		}
		record := execution.Record{Job: child.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now().UTC(), Counters: execution.Counters{Total: int64(objectsPerChild), TotalBytes: int64(objectsPerChild) * 1024, Succeeded: int64(objectsPerChild), SucceededBytes: int64(objectsPerChild) * 1024}}
		for _, item := range child.Job.Items {
			record.Results = append(record.Results, execution.ItemResult{ID: item.ID, Status: execution.ItemSucceeded})
		}
		observations[i] = Observation{Number: i + 1, Record: record}
	}
	return p, observations
}

func TestObservationPageUsesOneDurableCommitAndPreservesEveryResult(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.db")
			db, repo := openTest(t, path)
			defer func() { _ = db.Close() }()
			p, observations := observationPage(t, repo, 64, 2)
			// Include a failure, so preservation covers the indexed failure report too.
			failed := &observations[63].Record
			failed.Status, failed.Results[0].Status, failed.Results[0].Error = execution.PartialFailed, execution.ItemFailed, "destination permission denied"
			failed.Counters.Succeeded, failed.Counters.SucceededBytes = 1, 1024
			failed.Counters.Failed, failed.Counters.FailedBytes = 1, 1024
			before := durableTransactionID(t, path)
			if batch {
				if err := repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, observation := range observations {
					if err := repo.Observe(p.ID, p.Generation, observation.Number, observation.Record); err != nil {
						t.Fatal(err)
					}
				}
			}
			wantCommits := 64
			if batch {
				wantCommits = 1
			}
			if commits := durableTransactionID(t, path) - before; commits != wantCommits {
				t.Fatalf("durable commits = %d, want %d", commits, wantCommits)
			}
			// A page containing only duplicate outcomes must not create an empty commit.
			assertNoTaskCommit(t, path, func() error { return repo.ObserveBatch(p.ID, p.Generation, observations) })
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, repo = openTest(t, path)
			children, err := repo.Children(p.ID, 0, 128)
			if err != nil || len(children) != 64 {
				t.Fatalf("children after restart: %d %v", len(children), err)
			}
			for i, child := range children {
				if child.Record == nil || !reflect.DeepEqual(*child.Record, observations[i].Record) {
					t.Fatalf("child %d lost its exact outcome after restart", i+1)
				}
			}
			current, err := repo.Get(p.ID)
			if err != nil || current.Counters.Total != 128 || current.Counters.Succeeded != 127 || current.Counters.Failed != 1 || current.FinishedChildren != 64 || current.FailedChildren != 1 {
				t.Fatalf("parent counters: %+v %v", current, err)
			}
			outstanding, err := repo.Outstanding(p.ID, 0, 128)
			if err != nil || len(outstanding) != 0 {
				t.Fatalf("completed children remained outstanding: %d %v", len(outstanding), err)
			}
			var failures []Exception
			if err := repo.VisitExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}, func(e Exception) error { failures = append(failures, e); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(failures) != 1 || failures[0].Key != "child-63/object-0" || failures[0].Error != failed.Results[0].Error {
				t.Fatalf("failure report changed: %+v", failures)
			}
		})
	}
}

func TestObservationPageRollsBackAllChangesAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	p, observations := observationPage(t, repo, 3, 1)
	original := observations[2].Record.Job.ChildID
	observations[2].Record.Job.ChildID = "unexpected"
	before := databaseDigest(t, path)
	if err := repo.ObserveBatch(p.ID, p.Generation, observations); !errors.Is(err, ErrConflict) {
		t.Fatalf("malformed child result was accepted: %v", err)
	}
	if databaseDigest(t, path) != before {
		t.Fatal("part of rejected page was persisted")
	}
	current, _ := repo.Get(p.ID)
	if current.Counters.Total != 0 || current.FinishedChildren != 0 {
		t.Fatal("rejected page changed parent counters", current)
	}
	children, err := repo.Children(p.ID, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child.Record != nil || child.Accepted {
			t.Fatal("rejected page changed a child", child)
		}
	}
	observations[2].Record.Job.ChildID = original
	// Valid repeated and older outcomes within one page must not double-count.
	older := observations[0]
	older.Record.UpdatedAt = older.Record.UpdatedAt.Add(-time.Second)
	observations = append(observations, observations[0], older)
	if err := repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
		t.Fatal(err)
	}
	current, _ = repo.Get(p.ID)
	if current.Counters.Succeeded != 3 || current.FinishedChildren != 3 {
		t.Fatal("retry or duplicates changed counters", current)
	}
	before = databaseDigest(t, path)
	if err := repo.ObserveBatch(p.ID, p.Generation+1, observations); !errors.Is(err, ErrGeneration) {
		t.Fatal("late generation accepted", err)
	}
	if databaseDigest(t, path) != before {
		t.Fatal("generation rejection wrote the database")
	}
}

func BenchmarkObservationPage(b *testing.B) {
	for _, batch := range []bool{false, true} {
		b.Run(fmt.Sprintf("batch=%v", batch), func(b *testing.B) {
			db, err := state.Open(filepath.Join(b.TempDir(), "master.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			repo := New(db)
			p, observations := observationPage(b, repo, 64, 128)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range observations {
					observations[j].Record.UpdatedAt = time.Now().UTC()
				}
				if batch {
					if err := repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
						b.Fatal(err)
					}
				} else {
					for _, observation := range observations {
						if err := repo.Observe(p.ID, p.Generation, observation.Number, observation.Record); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}
