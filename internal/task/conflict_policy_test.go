package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// OVERWRITE_CHANGED is a stored task policy that reaches every child job;
// any other value is rejected.
func TestConflictPolicyIsValidatedAndReachesChildren(t *testing.T) {
	for _, policy := range []string{"", "skip", "CHANGED"} {
		spec := testSpec()
		spec.ConflictPolicy = policy
		if err := validate(spec); err == nil {
			t.Errorf("conflict policy %q accepted", policy)
		}
	}
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	// Windows cannot remove the temporary directory while the database is open.
	defer db.Close()
	for _, policy := range []string{execution.ConflictSkip, execution.ConflictOverwrite, execution.ConflictOverwriteChanged} {
		spec := testSpec()
		spec.ConflictPolicy = policy
		p, _, err := repo.Create(spec, "request-"+policy)
		if err != nil {
			t.Fatal(policy, err)
		}
		child, err := repo.Assign(p.ID, p.Generation, 1, objects("key-"+policy))
		if err != nil {
			t.Fatal(policy, err)
		}
		if child.Job.ConflictPolicy != policy {
			t.Fatalf("child policy %q, want %q", child.Job.ConflictPolicy, policy)
		}
	}
}
