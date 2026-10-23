package task

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func schedulingObjects(prefix string, count int, size int64) []transferstore.Object {
	items := make([]transferstore.Object, count)
	for i := range items {
		items[i] = transferstore.Object{Key: fmt.Sprintf("%s-%d", prefix, i), Size: size}
	}
	return items
}

func schedulingReport(child Child, succeeded int) execution.Record {
	r := execution.Record{Job: child.Job, Status: execution.Running, Attempt: 1, UpdatedAt: time.Now().UTC()}
	r.Counters.Total = int64(len(child.Job.Items))
	for i, item := range child.Job.Items {
		r.Counters.TotalBytes += item.Source.Size
		if i < succeeded {
			r.Counters.Succeeded++
			r.Counters.SucceededBytes += item.Source.Size
		}
	}
	if succeeded == len(child.Job.Items) {
		r.Status = execution.Completed
	}
	return r
}

func TestBalancedPlacementUsesRemainingWorkAcrossRestartAndLegacyRebuild(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", rebuild), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.db")
			db, repo := openTest(t, path)
			defer func() { _ = db.Close() }()
			p, _, err := repo.Create(testSpec(), "load")
			if err != nil {
				t.Fatal(err)
			}
			a, err := repo.Assign(p.ID, p.Generation, 1, schedulingObjects("large-batch", 128, 1<<20))
			if err != nil {
				t.Fatal(err)
			}
			b, err := repo.Assign(p.ID, p.Generation, 2, schedulingObjects("other-batch", 16, 1<<20))
			if err != nil || a.Node.ID == b.Node.ID {
				t.Fatal("initial work did not spread", err)
			}
			if err := repo.Observe(p.ID, p.Generation, a.Job.Number, schedulingReport(a, 120)); err != nil {
				t.Fatal(err)
			}
			if rebuild {
				// Simulate opening an older database, whose pending-load summary
				// did not exist. Its immutable children and reports are sufficient.
				if err := db.Update(func(tx *state.Tx) error {
					p, err := get(tx, p.ID)
					if err != nil {
						return err
					}
					p.Scheduling = nil
					return save(tx, p)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, repo = openTest(t, path)
			c, err := repo.Assign(p.ID, p.Generation, 3, schedulingObjects("next", 1, 1<<20))
			if err != nil || c.Node.ID != a.Node.ID {
				t.Fatalf("remaining 8 MiB should beat 16 MiB despite historical 128 MiB: node=%s err=%v", c.Node.ID, err)
			}
			if err := repo.Observe(p.ID, p.Generation, b.Job.Number, schedulingReport(b, 16)); err != nil {
				t.Fatal(err)
			}
			d, err := repo.Assign(p.ID, p.Generation, 4, schedulingObjects("idle-node", 1, 1<<20))
			if err != nil || d.Node.ID != b.Node.ID {
				t.Fatalf("completed work still reserved node capacity: node=%s err=%v", d.Node.ID, err)
			}
			if replay, err := repo.Assign(p.ID, p.Generation, 1, schedulingObjects("large-batch", 128, 1<<20)); err != nil || replay.Node != a.Node {
				t.Fatal("new observations changed an existing assignment", err)
			}
		})
	}
}

func TestBalancedPlacementAccountsForOtherActiveTasksAndTinyObjects(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	first, _, err := repo.Create(testSpec(), "first")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := repo.Assign(first.ID, 1, 1, schedulingObjects("zero-byte", 128, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		older, err := get(tx, first.ID)
		if err != nil {
			return err
		}
		older.Scheduling = nil
		return save(tx, older)
	}); err != nil {
		t.Fatal(err)
	}
	second, _, err := repo.Create(testSpec(), "second")
	if err != nil {
		t.Fatal(err)
	}
	placed, err := repo.Assign(second.ID, 1, 1, schedulingObjects("tiny", 1, 1))
	if err != nil || placed.Node.ID == busy.Node.ID {
		t.Fatal("another parent's zero-byte backlog was treated as an idle node", err)
	}
	older, err := repo.Get(first.ID)
	if err != nil || older.Scheduling == nil || older.Scheduling.Nodes[busy.Node.ID].Objects != 128 {
		t.Fatal("cross-task legacy load was not persisted and would be rebuilt for every dispatch page", err)
	}
	if _, err := repo.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(first.ID, 1); err != nil {
		t.Fatal(err)
	}
	third, _, err := repo.Create(testSpec(), "third")
	if err != nil {
		t.Fatal(err)
	}
	placed, err = repo.Assign(third.ID, 1, 1, schedulingObjects("after-stop", 1, 1))
	if err != nil || placed.Node.ID != busy.Node.ID {
		t.Fatal("stopped work continued reserving execution capacity", err)
	}
}

func TestResumedLoadRetainsSuccessesAndRequeuesFailedObjects(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "resume-load")
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Assign(p.ID, 1, 1, schedulingObjects("mixed", 128, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	report := schedulingReport(child, 120)
	report.Status = execution.PartialFailed
	report.Counters.Failed, report.Counters.FailedBytes = 8, 8<<20
	if err := repo.Observe(p.ID, 1, 1, report); err != nil {
		t.Fatal(err)
	}
	if err := repo.ScanProgress(p.ID, 1, 128, 128<<20, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.FinishIfReady(p.ID, 1); err != nil || !done {
		t.Fatal("parent did not finish", err)
	}
	p, err = repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	// A separate task must avoid the node with the resumed eight-object tail,
	// even before the resumed Agent has delivered its next progress report.
	other, _, err := repo.Create(testSpec(), "during-resume")
	if err != nil {
		t.Fatal(err)
	}
	placed, err := repo.Assign(other.ID, 1, 1, schedulingObjects("new", 1, 1))
	if err != nil || placed.Node.ID == child.Node.ID {
		t.Fatal("resumed failed tail was not reserved", err)
	}
	// Trigger the resumed parent's lazy rebuild and check that its 120 durable
	// successes are not charged as pending work a second time.
	if _, err := repo.Assign(p.ID, p.Generation, 2, schedulingObjects("new-scan-tail", 1, 1)); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	load := p.Scheduling.Nodes[child.Node.ID]
	if load.Objects != 8 || load.Bytes != 8<<20 || load.Children != 1 {
		t.Fatalf("incorrect retry reservation: %+v", load)
	}
}
