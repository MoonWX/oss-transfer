package execution

import (
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestChildTimingAndAdmissionUsageSurviveRecovery(t *testing.T) {
	f := newFixture(t, 1)
	r, err := f.repo.Accept(bg, f.job)
	if err != nil || r.CreatedAt.IsZero() || r.StartedAt != nil || r.FinishedAt != nil {
		t.Fatal(r, err)
	}
	created := r.CreatedAt
	if n, capacity, err := f.repo.Unfinished(); err != nil || n != 1 || capacity != 1024 {
		t.Fatal(n, capacity, err)
	}
	r, err = f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil || r.StartedAt == nil || r.StartedAt.Before(created) {
		t.Fatal(r, err)
	}
	started := *r.StartedAt
	if err = f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Recover(bg); err != nil {
		t.Fatal(err)
	}
	r, err = f.repo.begin(bg, f.job.ChildID, 1, true, false)
	if err != nil || !r.CreatedAt.Equal(created) || r.StartedAt == nil || !r.StartedAt.Equal(started) || r.FinishedAt != nil {
		t.Fatal(r, err)
	}
	r, err = f.repo.commit(bg, f.job.ChildID, 1, r.Attempt, []ItemResult{{ID: f.job.Items[0].ID, Status: ItemSkipped}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = f.repo.finish(bg, f.job.ChildID, 1, r.Attempt, nil)
	if err != nil || r.Status != Completed || r.FinishedAt == nil || r.FinishedAt.Before(started) {
		t.Fatal(r, err)
	}
	if n, _, err := f.repo.Unfinished(); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
