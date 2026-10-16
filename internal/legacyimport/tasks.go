package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func rawRecordKey(node, key string) string {
	sum := sha256.Sum256([]byte(node + "\x00" + key))
	return hex.EncodeToString(sum[:])
}
func oldTaskKey(node string, id int64) string { return node + "\x00" + strconv.FormatInt(id, 10) }
func taskRecordCategory(c string) bool {
	switch c {
	case "tasks", "task-catalogue", "recycle-bin", "purge-fences", "stop-fences", "completion-ledger", "file-report-ledger", "inventory", "discovery", "task-bandwidth", "exceptions", "temporary-files":
		return true
	}
	return false
}
func (p *Prepared) sourceRecord(node, key string) (oldRawRecord, error) {
	var row oldRawRecord
	err := p.Master.View(func(tx *state.Tx) error {
		found, err := tx.Get("legacy-source-records", rawRecordKey(node, key), &row)
		if err != nil {
			return err
		}
		if !found || row.Node != node || row.Key != key {
			return errors.New("indexed legacy source record is missing")
		}
		return nil
	})
	return row, err
}
func (p *Prepared) saveSourceRecords(rows []oldRawRecord) error {
	return p.Master.Update(func(tx *state.Tx) error {
		for _, row := range rows {
			key := rawRecordKey(row.Node, row.Key)
			var old oldRawRecord
			found, err := tx.Get("legacy-source-records", key, &old)
			if err != nil {
				return err
			}
			if found && (old.Node != row.Node || old.Key != row.Key || string(old.Value) != string(row.Value)) {
				return errors.New("legacy snapshot record changed during conversion")
			}
			if !found {
				if err := tx.Put("legacy-source-records", key, row); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func parseOldID(text string) (int64, error) {
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != text {
		return 0, errors.New("invalid legacy task identity")
	}
	return id, nil
}
func (c *taskCatalogue) group(id int64) (*oldGroup, error) {
	if id <= 0 || id > 1<<53-2 {
		return nil, errors.New("legacy public task ID must be positive and exactly representable")
	}
	if c.Groups[id] == nil {
		c.Groups[id] = &oldGroup{ID: id}
	}
	c.HighWater = max(c.HighWater, uint64(id))
	return c.Groups[id], nil
}
func (p *Prepared) collectTasks(ctx context.Context) (*taskCatalogue, error) {
	c := &taskCatalogue{Groups: map[int64]*oldGroup{}, Tasks: map[string]*oldTaskRef{}, CreationTasks: map[int64]int64{}, Creations: map[string]int64{}, Inventories: map[string][]int64{}, Fences: map[int64]bool{}, TaskFences: map[int64]bool{}, AuthoritativeFences: map[int64]bool{}, ScopedParents: map[string][]int64{}, Policies: map[string]oldBandwidth{}}
	for _, source := range p.Manifest.Sources {
		var batch []oldRawRecord
		var batchBytes int
		err := p.Iterate(ctx, source.Node, func(key, value []byte) error {
			k := string(key)
			if taskRecordCategory(category(k)) {
				batch = append(batch, oldRawRecord{source.Node, k, append([]byte(nil), value...)})
				batchBytes += len(value) + len(key)
				if len(batch) == 128 || batchBytes >= 8<<20 {
					if err := p.saveSourceRecords(batch); err != nil {
						return err
					}
					batch = nil
					batchBytes = 0
				}
			}
			switch {
			case k == "task:id:sequence":
				var id int64
				if json.Unmarshal(value, &id) != nil || id < 0 {
					return errors.New("invalid legacy task allocation sequence")
				}
				c.HighWater = max(c.HighWater, uint64(id))
			case strings.HasPrefix(k, "task_creation:") && source.Role == "master":
				var creation oldCreation
				if json.Unmarshal(value, &creation) != nil || creation.ID != strings.TrimPrefix(k, "task_creation:") {
					return errors.New("invalid legacy creation")
				}
				if creation.PublicTaskID == nil && len(creation.TaskIDs) > 0 {
					creation.PublicTaskID = &creation.TaskIDs[0]
				}
				if creation.PublicTaskID == nil {
					return errors.New("legacy creation has no allocated public ID; retain snapshot and finish or cancel its initial allocation before migration")
				}
				g, err := c.group(*creation.PublicTaskID)
				if err != nil {
					return err
				}
				if g.Creation != nil {
					return errors.New("multiple legacy creations claim one public task")
				}
				g.Creation = &creation
				g.SourceNode, g.SourceKey = source.Node, k
				g.Deleted = creation.Deleted
				c.Creations[creation.ID] = g.ID
				for _, id := range creation.TaskIDs {
					if previous := c.CreationTasks[id]; previous != 0 && previous != g.ID {
						return errors.New("legacy execution task belongs to multiple parents")
					}
					c.CreationTasks[id] = g.ID
					if id > 0 {
						c.HighWater = max(c.HighWater, uint64(id))
					}
				}
			case strings.HasPrefix(k, "local:parent:") && source.Role == "master":
				tail := strings.TrimPrefix(k, "local:parent:")
				if strings.HasPrefix(tail, "purged:") {
					id, err := parseOldID(strings.TrimPrefix(tail, "purged:"))
					if err != nil {
						return err
					}
					var yes bool
					if json.Unmarshal(value, &yes) != nil {
						return errors.New("invalid purge fence")
					}
					if yes {
						c.Fences[id] = true
						c.AuthoritativeFences[id] = true
					}
					return nil
				}
				if strings.HasPrefix(tail, "deleted:") {
					id, err := parseOldID(strings.TrimPrefix(tail, "deleted:"))
					if err != nil {
						return err
					}
					g, err := c.group(id)
					if err != nil {
						return err
					}
					var row oldTask
					if json.Unmarshal(value, &row) != nil || row.TaskIndex != id {
						return errors.New("invalid recycled legacy parent")
					}
					g.Snapshot = &row
					g.Deleted = true
					return nil
				}
				if strings.HasPrefix(tail, "id:") {
					return nil
				}
				id, err := parseOldID(tail)
				if err != nil {
					return err
				}
				var parent oldLocalParent
				if json.Unmarshal(value, &parent) != nil || parent.ParentTaskID != id {
					return errors.New("invalid local parent identity")
				}
				g, err := c.group(id)
				if err != nil {
					return err
				}
				if g.Local != nil {
					return errors.New("duplicate local parent")
				}
				g.Local = &parent
				g.SourceNode, g.SourceKey = source.Node, k
			case strings.HasPrefix(k, "task:purged:") || strings.HasPrefix(k, "report:purged:"):
				tail := strings.TrimPrefix(strings.TrimPrefix(k, "task:purged:"), "report:purged:")
				id, err := parseOldID(tail)
				if err != nil {
					return err
				}
				var yes bool
				if json.Unmarshal(value, &yes) != nil {
					return errors.New("invalid legacy purge fence")
				}
				if yes && id > 0 {
					c.Fences[id] = true
					if strings.HasPrefix(k, "task:purged:") {
						c.TaskFences[id] = true
					}
					c.HighWater = max(c.HighWater, uint64(id))
				}
			case category(k) == "tasks" || strings.HasPrefix(k, "task:deleted:"):
				var row oldTask
				if json.Unmarshal(value, &row) != nil {
					return errors.New("invalid legacy execution record")
				}
				id, err := parseOldID(strings.TrimPrefix(strings.TrimPrefix(k, "task:deleted:"), "task:"))
				if err != nil || row.TaskIndex != id {
					return errors.New("legacy task key and identity differ")
				}
				ref := &oldTaskRef{Node: source.Node, Key: k, Value: row, Deleted: strings.HasPrefix(k, "task:deleted:")}
				mapKey := oldTaskKey(source.Node, id)
				if old := c.Tasks[mapKey]; old != nil {
					if old.Deleted || !ref.Deleted {
						return errors.New("duplicate legacy task state")
					}
				}
				c.Tasks[mapKey] = ref
				if id > 0 {
					c.HighWater = max(c.HighWater, uint64(id))
				}
			case strings.HasPrefix(k, "task_bandwidth:desired:") && source.Role == "master":
				var policy oldBandwidth
				if json.Unmarshal(value, &policy) != nil || policy.ScopeID != strings.TrimPrefix(k, "task_bandwidth:desired:") {
					return errors.New("invalid legacy bandwidth policy")
				}
				c.Policies[policy.ScopeID] = policy
			}
			if len(c.Groups)+len(c.Tasks) > maxFacts {
				return errors.New("legacy task catalogue exceeds conversion capacity")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := p.saveSourceRecords(batch); err != nil {
			return nil, err
		}
	}
	// Master catalogue is authoritative. Node-local identities are paired with the
	// node; a negative internal ID is never used as a new public parent ID.
	retainedParents := map[int64]bool{}
	for _, ref := range c.Tasks {
		id := c.CreationTasks[ref.Value.TaskIndex]
		if ref.Value.ParentTaskID != nil && *ref.Value.ParentTaskID > 0 {
			if id != 0 && id != *ref.Value.ParentTaskID {
				return nil, errors.New("legacy parent identity disagrees with creation catalogue")
			}
			id = *ref.Value.ParentTaskID
		}
		if id == 0 {
			id = ref.Value.TaskIndex
		}
		g, err := c.group(id)
		if err != nil {
			return nil, err
		}
		ref.ParentID = id
		retainedParents[id] = true
		if ref.Value.TaskIndex == id && ref.Value.ParentTaskID != nil && *ref.Value.ParentTaskID == id && ref.Value.ShardIndex == nil && ref.Value.ShardID == "" {
			if g.Snapshot == nil {
				row := ref.Value
				g.Snapshot = &row
			}
			continue
		}
		if ref.Value.ShardID == "discovery" || ref.Value.ShardID == "orchestration" {
			continue
		}
		g.Children = append(g.Children, ref)
		if g.SourceNode == "" {
			g.SourceNode, g.SourceKey = ref.Node, ref.Key
		}
	}
	for id := range c.Fences {
		parent := c.CreationTasks[id]
		if parent == 0 && c.Groups[id] != nil && c.Groups[id].Creation != nil {
			parent = id
		}
		if parent != 0 {
			g := c.Groups[parent]
			// Older creations may retain deleted=false after every execution was
			// permanently removed. Accept that stale catalogue only after scanning
			// finished and no dispatch remains. A report fence alone, or any task
			// record on any source node, cannot prove an entire creation was purged.
			scanFinished := g.Creation.ScanComplete != nil && *g.Creation.ScanComplete && len(g.Creation.PendingDispatches) == 0
			allPurged := len(g.Creation.TaskIDs) > 0 && !retainedParents[parent] && g.Snapshot == nil && (g.Deleted || scanFinished)
			for _, executionID := range g.Creation.TaskIDs {
				allPurged = allPurged && c.TaskFences[executionID]
			}
			for _, node := range g.Creation.ParticipatingWorkerNodes {
				_, present := p.plans[node]
				allPurged = allPurged && present
			}
			if allPurged {
				g.Purged, g.Deleted = true, true
			} else {
				addBlocker(g, "EXECUTION_PURGE_FENCE_REQUIRES_REVIEW")
			}
			continue
		}
		g, err := c.group(id)
		if err != nil {
			return nil, err
		}
		if g.Local != nil && !c.AuthoritativeFences[id] {
			addBlocker(g, "EXECUTION_PURGE_FENCE_REQUIRES_REVIEW")
			continue
		}
		g.Purged, g.Deleted = true, true
	}
	return c, nil
}

// ConvertTasks retains historical parents, children and object records. It does
// not turn V4 progress markers into Go completion metadata or release the
// migration startup fence. Every unfinished parent remains stopped.
func (p *Prepared) ConvertTasks(ctx context.Context, options TaskImportOptions) (TaskConversion, error) {
	result := TaskConversion{MigrationID: p.Manifest.MigrationID, Blockers: []TaskBlocker{}}
	if p.closed || p.Master == nil {
		return result, errors.New("migration preparation is closed")
	}
	c, err := p.collectTasks(ctx)
	if err != nil {
		return result, err
	}
	ids := make([]int64, 0, len(c.Groups))
	for id := range c.Groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	repo := task.New(p.Master)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		row, err := p.convertParent(c, c.Groups[id], options)
		if err != nil {
			return result, fmt.Errorf("convert parent %d: %w", id, err)
		}
		if err := repo.ImportLegacy(task.LegacyImport{MigrationID: p.Manifest.MigrationID, HighWater: c.HighWater, Parents: []task.LegacyParentImport{row.Import}}); err != nil {
			return result, err
		}
		result.Parents++
		result.Children += len(row.Import.Children)
		for _, code := range row.Import.Parent.Legacy.Blockers {
			result.Blockers = append(result.Blockers, TaskBlocker{id, code})
		}
	}
	// Reserve allocated IDs even when only a sequence or permanent fence remains.
	if err := repo.ImportLegacy(task.LegacyImport{MigrationID: p.Manifest.MigrationID, HighWater: c.HighWater}); err != nil {
		return result, err
	}
	history, err := p.convertHistory(ctx, c, repo)
	if err != nil {
		return result, err
	}
	result.HistoryRecords = history
	result.HighWater = c.HighWater
	result.CatalogueComplete = true
	if err := p.Master.Update(func(tx *state.Tx) error { return tx.Put("migration-sections", "task-catalogue", result) }); err != nil {
		return result, err
	}
	return result, nil
}
