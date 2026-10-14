package legacyimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func finalizedFixture(t *testing.T) *Prepared {
	t.Helper()
	f := taskFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	settings := map[string]map[string]string{}
	for _, node := range p.Manifest.Nodes {
		settings[node.LegacyNode] = effectiveSettings()
	}
	if err := p.WriteRuntimeConfig(context.Background(), ":1685", settings); err != nil {
		t.Fatal(err)
	}
	if err := p.AcceptRuntimeDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestFinalizeAtomicPublicationPreservesRawAndAllowsOnlyFinalPath(t *testing.T) {
	requirePublicationPlatform(t)
	p := finalizedFixture(t)
	stage := p.Directory
	if err := p.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Directory == stage || p.Manifest.Pending {
		t.Fatal("not published")
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging left runnable", err)
	}
	for _, source := range p.Manifest.Sources {
		if _, err := os.Stat(filepath.Join(p.Directory, source.Path)); err != nil {
			t.Fatal("raw evidence lost", err)
		}
	}
	db, err := state.Open(filepath.Join(p.Directory, "master", "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	copied := filepath.Join(t.TempDir(), "copied")
	if err := os.CopyFS(copied, os.DirFS(p.Directory)); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(filepath.Join(copied, "master", "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("prepared database started outside final destination", err)
	}
}
func TestFinalizeRetryAfterPartialFenceCommitAndDestinationCollision(t *testing.T) {
	requirePublicationPlatform(t)
	p := finalizedFixture(t)
	directory := p.Directory
	if err := p.Master.Update(func(tx *state.Tx) error {
		return tx.Put("migration", "state", map[string]any{"migrationId": p.Manifest.MigrationID, "pending": false, "expectedDatabase": filepath.Join(p.Manifest.Destination, "master", "master.db")})
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("partly published database started", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPrepared(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := os.Mkdir(reopened.Manifest.Destination, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(reopened.Manifest.Destination, "keep")
	if err := os.WriteFile(marker, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Finalize(context.Background()); err == nil {
		t.Fatal("destination replaced")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("destination changed")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reopened.Manifest.Destination); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeRequiresExplicitRuntimeAcceptance(t *testing.T) {
	requirePublicationPlatform(t)
	p := finalizedFixture(t)
	if err := p.Master.Update(func(tx *state.Tx) error { return tx.Delete("migration-sections", "runtime-acceptance") }); err != nil {
		t.Fatal(err)
	}
	if err := p.Finalize(context.Background()); err == nil {
		t.Fatal("unaccepted execution defaults were published")
	}
	if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("failed preflight cleared startup fence", err)
	}
	status, err := p.Status()
	if err != nil || status.RuntimeAccepted || len(status.Remaining) != 1 {
		t.Fatal(status, err)
	}
	if err := p.AcceptRuntimeDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = p.Status()
	if err != nil || !status.RuntimeAccepted || len(status.Remaining) != 0 {
		t.Fatal(status, err)
	}
	if err := p.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func requirePublicationPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("successful atomic migration publication requires Linux or macOS; unsupported-platform refusal is tested separately")
	}
}
