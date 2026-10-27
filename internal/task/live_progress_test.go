package task

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func liveProgressReport(c Child, succeeded, inflight int64) ProgressReport {
	return ProgressReport{NodeID: c.Node.ID, EnrollmentID: c.Node.EnrollmentID, Child: execution.ChildProgress{
		ChildID: c.Job.ChildID, Number: c.Job.Number, Generation: c.Job.Generation, Assignment: c.Job.Assignment,
		Attempt: 1, SucceededBytes: succeeded, InFlightBytes: inflight, ActiveObjects: 1,
	}}
}

func TestProjectProgressCountsLargeObjectsAcrossNodesWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "live")
	if err != nil {
		t.Fatal(err)
	}
	const large int64 = 350 << 30
	first, err := repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "large", Size: large}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Assign(p.ID, p.Generation, 2, objects("small"))
	if err != nil || first.Node.ID == second.Node.ID {
		t.Fatal("expected two assigned nodes", first.Node, second.Node, err)
	}
	reports := []ProgressReport{liveProgressReport(second, 0, 512), liveProgressReport(first, 0, large/2)}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		view, err := repo.ProjectProgress(p.ID, p.Generation, reports)
		if err != nil || view.TotalBytes != large/2+512 || view.Parent.Counters.SucceededBytes != 0 || len(view.Children) != 2 ||
			view.Children[0].Progress.Number != 1 || view.Children[1].Progress.Number != 2 {
			t.Fatal(view, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("live progress query wrote to the database", err)
	}
	// The first file completes between collection of reports and projection.
	// Its old in-flight sample must not be credited on top of the new ledger.
	record := execution.Record{Job: first.Job, Attempt: 1, Status: execution.Completed, UpdatedAt: time.Now(),
		Counters: execution.Counters{Total: 1, TotalBytes: large, Succeeded: 1, SucceededBytes: large}}
	if err := repo.Observe(p.ID, p.Generation, first.Job.Number, record); err != nil {
		t.Fatal(err)
	}
	view, err := repo.ProjectProgress(p.ID, p.Generation, reports)
	if err != nil || view.TotalBytes != large+512 || len(view.Children) != 1 || view.Children[0].Progress.Number != 2 {
		t.Fatal("completed bytes were counted twice", view, err)
	}
}

func TestProjectProgressUsesOnlyUncreditedBytesAndFencesOldSamples(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "live")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "one", Size: 4096}, {Key: "two", Size: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	record := execution.Record{Job: c.Job, Attempt: 2, Status: execution.Running, UpdatedAt: time.Now(),
		Counters: execution.Counters{Total: 2, TotalBytes: 12288, Succeeded: 1, SucceededBytes: 4096}}
	if err := repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	report := liveProgressReport(c, 4096, 4096)
	report.Child.Attempt = 2
	view, err := repo.ProjectProgress(p.ID, p.Generation, []ProgressReport{report, report})
	if err != nil || view.TotalBytes != 8192 || len(view.Children) != 1 || view.Children[0].Bytes != 8192 {
		t.Fatal("duplicated a report or durable credit", view, err)
	}
	for _, test := range []struct {
		name   string
		change func(*ProgressReport)
	}{
		{"node", func(r *ProgressReport) { r.NodeID = "other" }},
		{"enrollment", func(r *ProgressReport) { r.EnrollmentID = "replaced" }},
		{"child", func(r *ProgressReport) { r.Child.ChildID = "other" }},
		{"number", func(r *ProgressReport) { r.Child.Number++ }},
		{"generation", func(r *ProgressReport) { r.Child.Generation++ }},
		{"assignment", func(r *ProgressReport) { r.Child.Assignment++ }},
		{"attempt", func(r *ProgressReport) { r.Child.Attempt-- }},
		{"unstarted", func(r *ProgressReport) { r.Child.Attempt = 0 }},
		{"old counters", func(r *ProgressReport) { r.Child.SucceededBytes = 0 }},
		{"negative bytes", func(r *ProgressReport) { r.Child.InFlightBytes = -1 }},
		{"negative success", func(r *ProgressReport) { r.Child.SucceededBytes = -1 }},
		{"object bound", func(r *ProgressReport) { r.Child.ActiveObjects = 129 }},
		{"sum overflow", func(r *ProgressReport) { r.Child.SucceededBytes, r.Child.InFlightBytes = math.MaxInt64, 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := report
			test.change(&invalid)
			view, err := repo.ProjectProgress(p.ID, p.Generation, []ProgressReport{invalid})
			if err != nil || view.TotalBytes != 4096 || len(view.Children) != 0 {
				t.Fatal("accepted stale or invalid sample", view, err)
			}
		})
	}
	if _, err := repo.ProjectProgress(p.ID, p.Generation+1, []ProgressReport{report}); !errors.Is(err, ErrGeneration) {
		t.Fatal("old parent generation accepted", err)
	}
	if _, err := repo.ProjectProgress(p.ID, p.Generation, make([]ProgressReport, MaxProgressReports+1)); err == nil {
		t.Fatal("unbounded progress reports accepted")
	}
	for _, status := range []execution.Status{execution.Completed, execution.PartialFailed, execution.Failed, execution.Stopped, execution.Recoverable, execution.Deleted} {
		t.Run(string(status), func(t *testing.T) {
			if err := db.Update(func(tx *state.Tx) error {
				var current Child
				if _, err := tx.Get("master-task-children", childKey(p.ID, 1), &current); err != nil {
					return err
				}
				current.Record.Status = status
				return tx.Put("master-task-children", childKey(p.ID, 1), current)
			}); err != nil {
				t.Fatal(err)
			}
			view, err := repo.ProjectProgress(p.ID, p.Generation, []ProgressReport{report})
			if err != nil || view.TotalBytes != 4096 || len(view.Children) != 0 {
				t.Fatal("inactive attempt retained live bytes", view, err)
			}
		})
	}
}

func TestProjectProgressRetryUsesOldGenerationCreditButNotItsAttempt(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "retry")
	if err != nil {
		t.Fatal(err)
	}
	// Inventory entries may initially have size zero. Progress uses the resolved
	// source size supplied by the current execution attempt.
	c, err := repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "one", Size: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		p.Generation, p.Counters.SucceededBytes = 2, 100
		c.Record = &execution.Record{Job: c.Job, Attempt: 40, Status: execution.PartialFailed, Counters: execution.Counters{SucceededBytes: 100}}
		c.Job.Generation = 2
		if err := tx.Put("master-task-children", childKey(p.ID, 1), c); err != nil {
			return err
		}
		return tx.Put("master-tasks", key(p.ID), p)
	}); err != nil {
		t.Fatal(err)
	}
	report := liveProgressReport(c, 100, 10<<30)
	view, err := repo.ProjectProgress(p.ID, 2, []ProgressReport{report})
	if err != nil || view.TotalBytes != 100+(10<<30) || len(view.Children) != 1 {
		t.Fatal("new generation was compared with an old attempt or unresolved size", view, err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		p.Status = "STOPPED"
		return tx.Put("master-tasks", key(p.ID), p)
	}); err != nil {
		t.Fatal(err)
	}
	view, err = repo.ProjectProgress(p.ID, 2, []ProgressReport{report})
	if err != nil || view.TotalBytes != 100 || len(view.Children) != 0 {
		t.Fatal("stopped parent retained live bytes", view, err)
	}
}
