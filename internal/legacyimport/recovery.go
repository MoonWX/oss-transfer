package legacyimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type RecoveryPreparation struct {
	MigrationID string `json:"migrationId"`
	Parents     int    `json:"parents"`
	Keys        int64  `json:"keys"`
	Protocol    int    `json:"protocol"`
}

func walkHistory(ctx context.Context, repo *task.Repository, id int64, kind string, fn func(task.LegacyHistory) error) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := repo.LegacyHistory(id, kind, after, 128)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := fn(row); err != nil {
				return err
			}
			after = row.Cursor
		}
		if len(rows) < 128 {
			return nil
		}
	}
}

// PrepareRecovery installs the immutable key union into the scanning Agent.
// It performs no storage I/O and leaves all imported tasks stopped.
func (p *Prepared) PrepareRecovery(ctx context.Context) (RecoveryPreparation, error) {
	result := RecoveryPreparation{MigrationID: p.Manifest.MigrationID, Protocol: 1}
	repo := task.New(p.Master)
	var after int64
	for {
		parents, err := repo.List(after, 128, false)
		if err != nil {
			return result, err
		}
		for _, parent := range parents {
			after = parent.ID
			if parent.Purged || parent.Legacy == nil || !parent.Legacy.NeedsReconciliation {
				continue
			}
			if len(parent.Spec.Nodes) == 0 {
				return result, fmt.Errorf("task %d has no scanning Agent", parent.ID)
			}
			var database *state.Store
			for _, identity := range p.Manifest.Nodes {
				if identity.InstanceID == parent.Spec.Nodes[0].ID {
					database = p.Agents[identity.LegacyNode]
				}
			}
			if database == nil {
				return result, errors.New("scanning Agent database is absent")
			}
			manifest := discovery.LegacyManifest{MigrationID: p.Manifest.MigrationID, ParentID: parent.ID}
			if err := walkHistory(ctx, repo, parent.ID, "INVENTORY", func(row task.LegacyHistory) error {
				if strings.Contains(row.Key, ":entry:") {
					manifest.Exact = true
				}
				return nil
			}); err != nil {
				return result, err
			}
			// Empty immutable inventories still carry the original ready marker.
			if err := walkHistory(ctx, repo, parent.ID, "INVENTORY", func(row task.LegacyHistory) error {
				if strings.HasSuffix(row.Key, ":ready") {
					manifest.Exact = true
				}
				return nil
			}); err != nil {
				return result, err
			}
			var pending []discovery.LegacyKey
			flush := func() error {
				if err := discovery.ImportLegacyKeys(database, manifest, pending); err != nil {
					return err
				}
				pending = nil
				return nil
			}
			add := func(row discovery.LegacyKey) error {
				pending = append(pending, row)
				if len(pending) == 128 {
					return flush()
				}
				return nil
			}
			if err := walkHistory(ctx, repo, parent.ID, "INVENTORY", func(row task.LegacyHistory) error {
				if !strings.Contains(row.Key, ":entry:") {
					return nil
				}
				var value struct{ Key string }
				if err := json.Unmarshal(row.Raw, &value); err != nil {
					return err
				}
				return add(discovery.LegacyKey{Key: value.Key})
			}); err != nil {
				return result, err
			}
			if err := flush(); err != nil {
				return result, err
			}
			addObserved := func(row discovery.LegacyKey) error {
				if manifest.Exact {
					found, err := discovery.LegacyHasKey(database, parent.ID, row.Key)
					if err != nil {
						return err
					}
					if !found {
						return errors.New("historical object is outside the immutable inventory")
					}
				}
				return add(row)
			}
			if err := walkHistory(ctx, repo, parent.ID, "OBJECT", func(row task.LegacyHistory) error {
				var value struct {
					SourceKey, SourcePath, LogicalKey, Result, ReasonCode string
					Size                                                  *int64
				}
				if err := json.Unmarshal(row.Raw, &value); err != nil {
					return err
				}
				key := value.SourceKey
				if parent.Spec.Source.Kind == "LOCAL" {
					var err error
					key, err = relativeLegacySource(parent, row.Node, value.SourcePath)
					if err != nil {
						return err
					}
				} else if key == "" {
					key = value.LogicalKey
				}
				return addObserved(discovery.LegacyKey{Key: key, Size: value.Size, Successful: value.Result == "SUCCESS", TimeFiltered: !manifest.Exact && value.Result == "SKIPPED" && value.ReasonCode == "TIME_FILTER"})
			}); err != nil {
				return result, fmt.Errorf("task %d object manifest: %w", parent.ID, err)
			}
			if err := walkHistory(ctx, repo, parent.ID, "COMPLETION", func(row task.LegacyHistory) error {
				var value struct {
					LogicalKey string
					Size       *int64
				}
				if err := json.Unmarshal(row.Raw, &value); err != nil {
					return err
				}
				key := value.LogicalKey
				if parent.Spec.Source.Kind == "LOCAL" {
					if parent.Spec.TargetKey != "" {
						if key != parent.Spec.TargetKey {
							return errors.New("completion key does not match exact destination")
						}
						key = parent.Spec.ObjectKey
						if key == "" {
							key = path.Base(strings.ReplaceAll(parent.Spec.Source.Root, "\\", "/"))
						}
					} else if parent.Spec.TargetPrefix != "" {
						prefix := parent.Spec.TargetPrefix
						if !strings.HasSuffix(prefix, "/") {
							prefix += "/"
						}
						if !strings.HasPrefix(key, prefix) {
							return errors.New("completion key is outside original target prefix")
						}
						key = strings.TrimPrefix(key, prefix)
					}
				}
				return addObserved(discovery.LegacyKey{Key: key, Size: value.Size, Successful: true})
			}); err != nil {
				return result, fmt.Errorf("task %d completion manifest: %w", parent.ID, err)
			}
			// Unfinished local batches can include objects not yet represented by an
			// object report. These source-relative keys belong to the same manifest.
			childAfter := 0
			for {
				rows, err := repo.LegacyChildren(parent.ID, childAfter, 128)
				if err != nil {
					return result, err
				}
				for _, row := range rows {
					var value struct {
						LocalFileEntries []struct {
							RelativeKey string
							Size        *int64
						}
					}
					if err := json.Unmarshal(row.Raw, &value); err != nil {
						return result, err
					}
					for _, entry := range value.LocalFileEntries {
						if err := add(discovery.LegacyKey{Key: entry.RelativeKey, Size: entry.Size}); err != nil {
							return result, err
						}
					}
					childAfter = row.Number
				}
				if len(rows) < 128 {
					break
				}
			}
			if err := flush(); err != nil {
				return result, err
			}
			manifest.Sealed = true
			if err := discovery.ImportLegacyKeys(database, manifest, nil); err != nil {
				return result, err
			}
			saved, err := discovery.LegacyManifestStatus(database, parent.ID)
			if err != nil {
				return result, err
			}
			if err := repo.ReadyLegacyRecovery(parent.ID, p.Manifest.MigrationID); err != nil {
				return result, err
			}
			result.Parents++
			result.Keys += saved.Count
		}
		if len(parents) < 128 {
			break
		}
	}
	err := p.Master.Update(func(tx *state.Tx) error { return tx.Put("migration-sections", "recovery-protocol", result) })
	return result, err
}

func relativeLegacySource(parent task.Parent, legacyNode, file string) (string, error) {
	// V4 report paths are lexical observations; never inspect the importing host's
	// filesystem to resolve a path which belongs to an execution node.
	root := parent.Spec.Source.Root
	for _, node := range parent.Spec.Nodes {
		if node.Address == legacyNode {
			root = node.LocalRoot
			break
		}
	}
	root = strings.ReplaceAll(root, "\\", "/")
	file = strings.ReplaceAll(file, "\\", "/")
	if root == "" || file == "" {
		return "", errors.New("local object report has no source path/root")
	}
	if file == root {
		return path.Base(file), nil
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(file, prefix) {
		return "", errors.New("local source report escapes original root")
	}
	key := strings.TrimPrefix(file, prefix)
	if key == "" || strings.HasPrefix(key, "/") || path.Clean(key) != key || key == ".." || strings.HasPrefix(key, "../") {
		return "", errors.New("invalid legacy relative source key")
	}
	return key, nil
}
