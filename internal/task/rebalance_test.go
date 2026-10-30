package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func rebalanceFixture(t *testing.T, mode string) (*state.Store, *Repository, Parent) {
	t.Helper()
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	t.Cleanup(func() { db.Close() })
	spec := testSpec()
	spec.DistributionMode = mode
	p, _ := createFailoverTask(t, repo, spec, 6)
	p = enableFailoverTest(t, db, repo, p, false)
	var err error
	p, err = repo.BeginFailover(p.ID, p.Generation, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = repo.PrepareFailover(p.ID, p.Generation, "b", map[string]bool{"a": true}); err != nil {
		t.Fatal(err)
	}
	var epoch uint64
	p, epoch, err = repo.BeginNodeRejoin(p.ID, p.Generation, "b")
	if err != nil {
		t.Fatal(err)
	}
	p, err = repo.FinishNodeRejoin(p.ID, p.Generation, "b", epoch)
	if err != nil {
		t.Fatal(err)
	}
	return db, repo, p
}

func TestQueuedRebalanceUsesPendingLoadAndPreservesAffinity(t *testing.T) {
	for _, mode := range []string{"BALANCED", "AFFINITY"} {
		t.Run(mode, func(t *testing.T) {
			_, repo, p := rebalanceFixture(t, mode)
			available := map[string]bool{"a": true, "b": true}
			moves := 0
			for range 20 {
				intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, available)
				if err != nil {
					t.Fatal(err)
				}
				if intent != nil {
					c, err := repo.FinishQueuedHandoff(p.ID, *intent, true, available)
					if err != nil {
						t.Fatal(err)
					}
					if c != nil {
						moves++
						if c.Node.ID != "b" || c.Job.Assignment != 1 || c.Job.OwnerEpoch != 1 {
							t.Fatal(c)
						}
					}
				}
				p, err = repo.Get(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if p.Failover.Rebalance == nil {
					break
				}
			}
			if mode == "BALANCED" && (moves != 3 || p.Scheduling.Nodes["a"] != p.Scheduling.Nodes["b"]) {
				t.Fatal("not balanced", moves, p.Scheduling)
			}
			if mode == "AFFINITY" && moves != 0 {
				t.Fatal("affinity changed", moves)
			}
		})
	}
}

func TestQueuedRebalanceSkipsRunningAndTracksOtherTaskLoad(t *testing.T) {
	_, repo, p := rebalanceFixture(t, "BALANCED")
	children, err := repo.Children(p.ID, 0, 6)
	if err != nil {
		t.Fatal(err)
	}
	running := schedulingReport(children[0], 0)
	if err := repo.Observe(p.ID, p.Generation, 1, running); err != nil {
		t.Fatal(err)
	}
	other, _, err := repo.Create(testSpec(), "busy-returner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AssignPageAvailable(other.ID, other.Generation, []Assignment{{Number: 1, Objects: schedulingObjects("busy", 128, 1<<20)}}, map[string]bool{"b": true}); err != nil {
		t.Fatal(err)
	}
	if intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, map[string]bool{"a": true, "b": true}); err != nil || intent != nil {
		t.Fatal("ignored other task load", intent, err)
	}
	if _, err := repo.Stop(other.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(other.ID, other.Generation); err != nil {
		t.Fatal(err)
	}
	intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, map[string]bool{"a": true, "b": true})
	if err != nil || intent == nil || intent.Number == 1 {
		t.Fatal("running child selected", intent, err)
	}
	if c, err := repo.FinishQueuedHandoff(p.ID, *intent, false, nil); err != nil || c != nil {
		t.Fatal(c, err)
	}
	rows, err := repo.Children(p.ID, 0, 6)
	if err != nil || rows[0].Record.Status != execution.Running || rows[0].Job.Assignment != 0 {
		t.Fatal("running child altered", err)
	}
}

func TestQueuedRebalanceIntentSurvivesStopResumeAndConcurrentFailover(t *testing.T) {
	_, repo, p := rebalanceFixture(t, "BALANCED")
	available := map[string]bool{"a": true, "b": true}
	intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, available)
	if err != nil || intent == nil {
		t.Fatal(intent, err)
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, _, err = repo.RequestResume(p.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PrepareResume(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	moved, err := repo.FinishQueuedHandoff(p.ID, *intent, true, available)
	if err != nil || moved == nil || moved.Job.Generation != p.Generation || moved.Job.Assignment != 1 {
		t.Fatal(moved, err)
	}
	if err := repo.Resumed(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	intent, err = repo.NextQueuedHandoff(p.ID, p.Generation, available)
	if err != nil || intent == nil {
		t.Fatal(intent, err)
	}
	if _, err := repo.BeginFailover(p.ID, p.Generation, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PrepareFailover(p.ID, p.Generation, "a", map[string]bool{"b": true}); err != nil {
		t.Fatal(err)
	}
	if intent, err = repo.NextQueuedHandoff(p.ID, p.Generation, map[string]bool{"b": true}); err != nil || intent != nil {
		t.Fatal("obsolete intent stuck after failover", intent, err)
	}
}

func TestQueuedRebalanceBackfillsPreviouslyRejoinedEpochOnlyOnce(t *testing.T) {
	db, repo, p := rebalanceFixture(t, "BALANCED")
	// This is the persisted format of a task rejoined by the preceding release.
	if err := db.Update(func(tx *state.Tx) error {
		current, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		current.Failover.Rebalance, current.Failover.RebalancedEpochs = nil, nil
		return repo.save(tx, current)
	}); err != nil {
		t.Fatal(err)
	}
	p, err := repo.Get(p.ID)
	if err != nil || !p.NeedsQueuedRebalance() {
		t.Fatal("previous returner was missed", err)
	}
	p, err = repo.EnsureQueuedRebalance(p.ID, p.Generation)
	if err != nil || p.Failover.Rebalance == nil {
		t.Fatal(err)
	}
	for range 20 {
		intent, err := repo.NextQueuedHandoff(p.ID, p.Generation, map[string]bool{"a": true, "b": true})
		if err != nil {
			t.Fatal(err)
		}
		if intent != nil {
			if _, err := repo.FinishQueuedHandoff(p.ID, *intent, true, map[string]bool{"a": true, "b": true}); err != nil {
				t.Fatal(err)
			}
		}
		p, err = repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p.Failover.Rebalance == nil {
			break
		}
	}
	if p.NeedsQueuedRebalance() || p.Failover.RebalancedEpochs["b"] != 1 {
		t.Fatal("completed queue pass repeated", p.Failover)
	}
	p, err = repo.EnsureQueuedRebalance(p.ID, p.Generation)
	if err != nil || p.Failover.Rebalance != nil || p.Scheduling.Nodes["b"].Children != 3 {
		t.Fatal("backfill not balanced or repeated", p, err)
	}
}
