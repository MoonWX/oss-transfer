package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func readPrivateJSON(path string, into any) error {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 4<<20 {
		return errors.New("migration file is missing, linked, or not private")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("migration file changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.New("invalid migration JSON file")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("migration JSON contains trailing data")
	}
	return nil
}
func openExistingPrivateStore(directory, relative string) (*state.Store, error) {
	current := directory
	path := filepath.Join(directory, filepath.FromSlash(relative))
	rel, err := filepath.Rel(directory, path)
	if err != nil || rel == ".." || filepath.IsAbs(rel) {
		return nil, errors.New("invalid migration database path")
	}
	for _, component := range splitComponents(rel) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("migration database path is missing, linked, or not private")
		}
		if current == path && !info.Mode().IsRegular() {
			return nil, errors.New("migration database is not a regular file")
		}
	}
	return state.Open(path)
}
func splitComponents(value string) []string { return strings.Split(value, string(filepath.Separator)) }
func verifyPending(db *state.Store, id string, expected ...string) error {
	return db.View(func(tx *state.Tx) error {
		var fence struct {
			MigrationID      string `json:"migrationId"`
			ExpectedDatabase string `json:"expectedDatabase,omitempty"`
			Pending          *bool  `json:"pending"`
		}
		found, err := tx.Get("migration", "state", &fence)
		if err != nil {
			return err
		}
		if !found || fence.Pending == nil || fence.MigrationID != id || (!*fence.Pending && (len(expected) != 1 || fence.ExpectedDatabase != expected[0])) {
			return errors.New("database is not part of this pending migration")
		}
		return nil
	})
}

// OpenPrepared reopens only an existing private, pending staging directory. All
// raw snapshots and identities are revalidated; no missing database is created.
func OpenPrepared(ctx context.Context, directory string) (_ *Prepared, resultErr error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("offline migration preparation currently requires a POSIX filesystem")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return nil, errors.New("staging directory must be an existing absolute path without symlinks")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("migration staging directory is not private")
	}
	var manifest PreparationManifest
	if err := readPrivateJSON(filepath.Join(absolute, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 || manifest.MigrationID != filepath.Base(absolute) || len(manifest.Sources) < 1 || len(manifest.Sources) > 10000 || len(manifest.Nodes) > 10000 {
		return nil, errors.New("invalid pending migration manifest")
	}
	if !filepath.IsAbs(manifest.Destination) || filepath.Clean(manifest.Destination) != manifest.Destination || filepath.Dir(manifest.Destination) != filepath.Dir(absolute) {
		return nil, errors.New("migration destination does not match staging location")
	}
	p := &Prepared{Directory: absolute, Manifest: manifest, Agents: map[string]*state.Store{}, plans: map[string]Plan{}, paths: map[string]string{}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, p.Close())
		}
	}()
	rawInfo, err := os.Lstat(filepath.Join(absolute, "raw"))
	if err != nil || !rawInfo.IsDir() || rawInfo.Mode()&os.ModeSymlink != 0 || rawInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("migration raw directory is missing, linked, or not private")
	}
	var plans []Plan
	for index, source := range manifest.Sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		expected := fmt.Sprintf("raw/%05d.jsonl", index+1)
		if source.Path != expected {
			return nil, errors.New("unexpected migration snapshot path")
		}
		path := filepath.Join(absolute, filepath.FromSlash(expected))
		before, err := os.Lstat(path)
		if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
			return nil, errors.New("snapshot is missing, linked, or not private")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(before, opened) {
			file.Close()
			return nil, errors.New("snapshot changed while opening")
		}
		hash := sha256.New()
		plan, readErr := Inspect(ctx, io.TeeReader(file, hash))
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != source.SHA256 || plan.Header.Node != source.Node || plan.Header.Role != source.Role || plan.Integrity != source.Footer {
			return nil, errors.New("snapshot integrity differs from manifest")
		}
		if _, ok := p.plans[source.Node]; ok {
			return nil, errors.New("duplicate migration node")
		}
		p.plans[source.Node], p.paths[source.Node] = plan, path
		plans = append(plans, plan)
	}
	combined, err := Combine(plans)
	if err != nil {
		return nil, err
	}
	for _, issue := range combined.Issues {
		if issue.Code == "NODE_SNAPSHOT_MISSING" || issue.Code == "CONNECTION_DRIFT" {
			return nil, errors.New("migration cluster coverage or configuration changed")
		}
	}
	p.Master, err = openExistingPrivateStore(absolute, "master/master.db")
	if err != nil {
		return nil, err
	}
	if err := verifyPending(p.Master, manifest.MigrationID, filepath.Join(manifest.Destination, "master", "master.db")); err != nil {
		return nil, err
	}
	validID := regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	for _, node := range manifest.Nodes {
		if !validID.MatchString(node.InstanceID) || node.Database != "agents/"+node.InstanceID+"/agent.db" || p.Agents[node.LegacyNode] != nil {
			return nil, errors.New("invalid migration Agent identity path")
		}
		if _, ok := p.plans[node.LegacyNode]; !ok {
			return nil, errors.New("Agent has no source snapshot")
		}
		db, err := openExistingPrivateStore(absolute, node.Database)
		if err != nil {
			return nil, err
		}
		p.Agents[node.LegacyNode] = db
		if err := verifyPending(db, manifest.MigrationID, filepath.Join(manifest.Destination, filepath.FromSlash(node.Database))); err != nil {
			return nil, err
		}
	}
	return p, nil
}

type PreparedStatus struct {
	Directory       string               `json:"directory"`
	MigrationID     string               `json:"migrationId"`
	Pending         bool                 `json:"pending"`
	Accounts        int                  `json:"accounts"`
	Connections     int                  `json:"connections"`
	TaskCatalogue   *TaskConversion      `json:"taskCatalogue,omitempty"`
	Recovery        *RecoveryPreparation `json:"recovery,omitempty"`
	RuntimeAccepted bool                 `json:"runtimeAccepted"`
	Remaining       []string             `json:"remaining"`
}

func (p *Prepared) Status() (PreparedStatus, error) {
	result := PreparedStatus{Directory: p.Directory, MigrationID: p.Manifest.MigrationID, Pending: true, Accounts: p.Manifest.Accounts, Connections: p.Manifest.Connections, Remaining: []string{}}
	if p.closed {
		return result, errors.New("migration preparation is closed")
	}
	err := p.Master.View(func(tx *state.Tx) error {
		var tasks TaskConversion
		found, err := tx.Get("migration-sections", "task-catalogue", &tasks)
		if err != nil {
			return err
		}
		if found {
			result.TaskCatalogue = &tasks
		}
		var recovery RecoveryPreparation
		found, err = tx.Get("migration-sections", "recovery-protocol", &recovery)
		if found {
			result.Recovery = &recovery
		}
		if err != nil {
			return err
		}
		var acceptance RuntimeAcceptance
		found, err = tx.Get("migration-sections", "runtime-acceptance", &acceptance)
		result.RuntimeAccepted = found && acceptance.Profile == RuntimeProfile && acceptance.MigrationID == p.Manifest.MigrationID
		return err
	})
	if !result.RuntimeAccepted {
		result.Remaining = append(result.Remaining, "explicit acceptance of Go runtime defaults")
	}
	configMissing := false
	for _, relative := range append([]string{"master/service.json"}, agentConfigPaths(p.Manifest.Nodes)...) {
		if _, statErr := os.Stat(filepath.Join(p.Directory, filepath.FromSlash(relative))); statErr != nil {
			configMissing = true
		}
	}
	if configMissing {
		result.Remaining = append([]string{"runtime configuration"}, result.Remaining...)
	}
	if result.Recovery == nil || result.Recovery.Protocol != 1 {
		result.Remaining = append(result.Remaining, "recovery protocol preparation")
	}
	if result.TaskCatalogue != nil {
		repo := task.New(p.Master)
		for _, blocker := range result.TaskCatalogue.Blockers {
			parent, readErr := repo.Get(blocker.ParentID)
			if readErr != nil {
				return result, readErr
			}
			if !parent.Purged && parent.Legacy != nil && parent.Legacy.NeedsReconciliation && !task.RecoveryBlockerSupported(blocker.Code) {
				result.Remaining = append(result.Remaining, fmt.Sprintf("task %d: %s", blocker.ParentID, blocker.Code))
			}
		}
	}
	if result.TaskCatalogue == nil {
		result.Remaining = append([]string{"task and report history conversion"}, result.Remaining...)
	}
	return result, err
}

func agentConfigPaths(nodes []NodeIdentity) []string {
	paths := make([]string, 0, len(nodes))
	for _, node := range nodes {
		paths = append(paths, filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(node.Database)), "service.json")))
	}
	return paths
}
