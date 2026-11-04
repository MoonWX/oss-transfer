package task

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// runningPage stores count outstanding children whose records are running
// with half of their objects done, as the observe loop sees them mid-task.
func runningPage(tb testing.TB, repo *Repository, count, objectsPerChild int) Parent {
	tb.Helper()
	p, observations := observationPage(tb, repo, count, objectsPerChild)
	for i := range observations {
		record := &observations[i].Record
		record.Status = execution.Running
		for j := range record.Results {
			if j%2 == 1 {
				record.Results[j].Status = execution.ItemPending
			}
		}
		record.Counters.Succeeded, record.Counters.SucceededBytes = int64(objectsPerChild/2), int64(objectsPerChild/2)*1024
	}
	if err := repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
		tb.Fatal(err)
	}
	return p
}

func TestOutstandingHeadsMatchFullRows(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p := runningPage(t, repo, 20, 4)
	// One child without a record yet.
	if _, err := repo.Assign(p.ID, p.Generation, 21, objects("unobserved")); err != nil {
		t.Fatal(err)
	}
	full, err := repo.Outstanding(p.ID, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	heads, err := repo.OutstandingHeads(p.ID, 0, 64)
	if err != nil || len(heads) != len(full) || len(full) != 21 {
		t.Fatal(len(heads), len(full), err)
	}
	for i, c := range full {
		h := heads[i]
		if h.Child.Record != nil || h.Child.Job.Items != nil {
			t.Fatal("head decoded bulk fields")
		}
		job := c.Job
		job.Items, job.Source, job.Destination, job.ConflictPolicy, job.RequestLimits = nil, execution.Endpoint{}, execution.Endpoint{}, "", nil
		if !jobsEqual(h.Child.Job, job) || h.Child.Node != c.Node || h.Child.Accepted != c.Accepted || h.HasRecord != (c.Record != nil) {
			t.Fatalf("head %d differs:\n%+v\n%+v", i, h.Child, c)
		}
		if c.Record != nil && (h.RecordAttempt != c.Record.Attempt || !h.RecordUpdatedAt.Equal(c.Record.UpdatedAt) || h.RecordStatus != c.Record.Status || h.RecordGeneration != c.Record.Job.Generation) {
			t.Fatalf("head %d record fields differ", i)
		}
	}
	page, err := repo.OutstandingHeads(p.ID, 10, 5)
	if err != nil || len(page) != 5 || page[0].Child.Job.Number != 11 {
		t.Fatal("head paging differs", len(page), err)
	}
	child, err := repo.Child(p.ID, 21)
	if err != nil || child.Job.Number != 21 || len(child.Job.Items) != 1 {
		t.Fatal(child, err)
	}
	if _, err := repo.Child(p.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func jobsEqual(a, b execution.Job) bool {
	return a.ChildID == b.ChildID && a.ParentID == b.ParentID && a.Number == b.Number && a.Generation == b.Generation && a.Assignment == b.Assignment && a.OwnerEpoch == b.OwnerEpoch && a.SharedLocalSource == b.SharedLocalSource && a.SharedLocalDestination == b.SharedLocalDestination && a.DiscoveryComplete == b.DiscoveryComplete
}

// BenchmarkOutstandingPage measures the Master's read of one 64-child observe
// page (128 objects per child, running records): full rows versus heads.
//
//	go test ./internal/task -run '^$' -bench OutstandingPage -benchmem
func BenchmarkOutstandingPage(b *testing.B) {
	db, repo := openTest(b, filepath.Join(b.TempDir(), "master.db"))
	defer db.Close()
	p := runningPage(b, repo, 64, 128)
	for _, mode := range []string{"full", "heads"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var err error
				if mode == "full" {
					_, err = repo.Outstanding(p.ID, 0, 64)
				} else {
					_, err = repo.OutstandingHeads(p.ID, 0, 64)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
