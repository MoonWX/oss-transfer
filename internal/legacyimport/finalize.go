package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/servicehost"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

var errUnsupportedPublication = errors.New("atomic migration publication is supported on Linux and macOS")

func publicationSupported() bool { return runtime.GOOS == "linux" || runtime.GOOS == "darwin" }

// Finalize publishes an offline conversion. It never resumes a task or accesses
// source/target storage. A DB whose fence is cleared during publication still
// refuses startup anywhere except its final destination, including staging.
func (p *Prepared) Finalize(ctx context.Context) error {
	if !publicationSupported() {
		return errUnsupportedPublication
	}
	if p.closed {
		return errors.New("migration preparation is closed")
	}
	if _, err := os.Lstat(p.Manifest.Destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("migration destination already exists or cannot be inspected")
	}
	// Re-read every private snapshot before publishing; no receipt overrides this.
	for _, source := range p.Manifest.Sources {
		if err := p.Iterate(ctx, source.Node, func([]byte, []byte) error { return nil }); err != nil {
			return err
		}
	}
	var tasks TaskConversion
	var recovery RecoveryPreparation
	if err := p.Master.View(func(tx *state.Tx) error {
		found, err := tx.Get("migration-sections", "task-catalogue", &tasks)
		if err != nil {
			return err
		}
		if !found || !tasks.CatalogueComplete || tasks.MigrationID != p.Manifest.MigrationID {
			return errors.New("task/history conversion is incomplete")
		}
		found, err = tx.Get("migration-sections", "recovery-protocol", &recovery)
		if err != nil {
			return err
		}
		if !found || recovery.Protocol != 1 || recovery.MigrationID != p.Manifest.MigrationID {
			return errors.New("recovery protocol is not prepared")
		}
		var acceptance RuntimeAcceptance
		found, err = tx.Get("migration-sections", "runtime-acceptance", &acceptance)
		if err != nil {
			return err
		}
		if !found || acceptance.MigrationID != p.Manifest.MigrationID || acceptance.Profile != RuntimeProfile {
			return errors.New("migration plan must explicitly accept Go execution defaults")
		}
		return nil
	}); err != nil {
		return err
	}
	masterConfig, err := servicehost.LoadConfig(filepath.Join(p.Directory, "master", "service.json"), "master")
	if err != nil {
		return err
	}
	if masterConfig.DataDirectory != filepath.Join(p.Manifest.Destination, "master") {
		return errors.New("master runtime config has an unexpected data directory")
	}
	databases := map[string]*state.Store{"master/master.db": p.Master}
	for _, node := range p.Manifest.Nodes {
		db := p.Agents[node.LegacyNode]
		if db == nil {
			return errors.New("Agent database is absent")
		}
		var identity agent.Identity
		var member cluster.Member
		if err := db.View(func(tx *state.Tx) error {
			found, err := tx.Get("agent-identity", "self", &identity)
			if err == nil && !found {
				return errors.New("Agent identity missing")
			}
			return err
		}); err != nil {
			return err
		}
		if err := p.Master.View(func(tx *state.Tx) error {
			found, err := tx.Get("agent-members", node.InstanceID, &member)
			if err == nil && !found {
				return errors.New("Agent membership missing")
			}
			return err
		}); err != nil {
			return err
		}
		credentialHash := sha256.Sum256([]byte(identity.Credential))
		if member.CredentialHash != hex.EncodeToString(credentialHash[:]) || identity.InstanceID != node.InstanceID || identity.EnrollmentID != node.EnrollmentID || identity.MasterURL != p.Manifest.PublicURL || identity.Credential == "" || member.ID != node.InstanceID || member.EnrollmentID != node.EnrollmentID || member.Address != node.Address || member.Revoked != node.Revoked {
			return errors.New("Agent identity no longer matches the migration manifest")
		}
		config, err := servicehost.LoadConfig(filepath.Join(p.Directory, filepath.Dir(filepath.FromSlash(node.Database)), "service.json"), "agent")
		if err != nil {
			return err
		}
		if config.DataDirectory != filepath.Join(p.Manifest.Destination, filepath.Dir(filepath.FromSlash(node.Database))) || config.NodeControl == nil {
			return errors.New("Agent runtime configuration is incomplete")
		}
		if _, err := config.NodeControl.Options(); err != nil {
			return err
		}
		databases[node.Database] = db
	}
	repo := task.New(p.Master)
	var after int64
	for {
		rows, err := repo.List(after, 128, false)
		if err != nil {
			return err
		}
		for _, row := range rows {
			after = row.ID
			if row.Purged || row.Legacy == nil || !row.Legacy.NeedsReconciliation {
				continue
			}
			if !task.Ended(row.Status) || !row.Legacy.RecoveryReady {
				return fmt.Errorf("task %d recovery protocol is not prepared", row.ID)
			}
			for _, blocker := range row.Legacy.Blockers {
				if !task.RecoveryBlockerSupported(blocker) {
					return fmt.Errorf("task %d: %s", row.ID, blocker)
				}
			}
			if len(row.Spec.Nodes) == 0 {
				return errors.New("recovery task has no scanning Agent")
			}
			var db *state.Store
			for _, node := range p.Manifest.Nodes {
				if node.InstanceID == row.Spec.Nodes[0].ID {
					db = p.Agents[node.LegacyNode]
				}
			}
			if db == nil {
				return errors.New("recovery scanning Agent absent")
			}
			manifest, err := discovery.LegacyManifestStatus(db, row.ID)
			if err != nil || !manifest.Sealed || manifest.MigrationID != p.Manifest.MigrationID {
				return fmt.Errorf("task %d manifest is not sealed", row.ID)
			}
		}
		if len(rows) < 128 {
			break
		}
	}
	// Each commit is restartable. OpenPrepared accepts this prepared-for-publish
	// state, while ClaimRole rejects it until the directory is atomically moved.
	for relative, db := range databases {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := db.Update(func(tx *state.Tx) error {
			return tx.Put("migration", "state", map[string]any{"migrationId": p.Manifest.MigrationID, "pending": false, "expectedDatabase": filepath.Join(p.Manifest.Destination, filepath.FromSlash(relative))})
		}); err != nil {
			return err
		}
	}
	p.Manifest.Pending = false
	temporary := filepath.Join(p.Directory, "manifest.publish.json")
	if err := writePrivateJSON(temporary, p.Manifest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		var prior PreparationManifest
		if err := readPrivateJSON(temporary, &prior); err != nil || !reflect.DeepEqual(prior, p.Manifest) {
			return errors.New("existing publish manifest differs")
		}
	}
	if err := os.Rename(temporary, filepath.Join(p.Directory, "manifest.json")); err != nil {
		return err
	}
	if err := syncDirectory(p.Directory); err != nil {
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	if err := publishDirectory(p.Directory, p.Manifest.Destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(p.Manifest.Destination)); err != nil {
		return err
	}
	p.Directory = p.Manifest.Destination
	return nil
}
