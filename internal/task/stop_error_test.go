package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestStopTransitionClearsOnlyObsoleteControlErrors(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "stop-errors")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *state.Tx) error {
		p.Error = "source listing failed"
		return repo.save(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RuntimeProblem(p.ID, p.Generation, "previous running control error"); err != nil {
		t.Fatal(err)
	}
	p, admitted, err := repo.RequestStop(p.ID)
	if err != nil || !admitted || p.RuntimeError != "" || p.Error != "source listing failed" {
		t.Fatal("accepted stop retained obsolete error or erased source failure", p, err)
	}
	if err := repo.RuntimeProblem(p.ID, p.Generation, "current stop failure"); err != nil {
		t.Fatal(err)
	}
	p, admitted, err = repo.RequestStop(p.ID)
	if err != nil || admitted || p.RuntimeError != "current stop failure" {
		t.Fatal("duplicate stop erased a current failure", p, err)
	}
	if err := repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, err = repo.Get(p.ID)
	if err != nil || p.Status != "STOPPED" || p.RuntimeError != "" || p.Error != "source listing failed" {
		t.Fatal("confirmed stop did not atomically clear obsolete control error", p, err)
	}
}
