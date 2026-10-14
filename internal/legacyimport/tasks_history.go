package legacyimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/task"
)

func (c *taskCatalogue) resolveParent(node string, id int64) int64 {
	if child := c.Tasks[oldTaskKey(node, id)]; child != nil {
		return child.ParentID
	}
	if parent := c.CreationTasks[id]; parent != 0 {
		return parent
	}
	if c.Groups[id] != nil {
		return id
	}
	return 0
}
func (c *taskCatalogue) childID(node string, parent, id int64, shard string) string {
	if child := c.Tasks[oldTaskKey(node, id)]; child != nil && child.Number > 0 && child.ParentID == parent {
		return fmt.Sprintf("%d-%d", parent, child.Number)
	}
	if group := c.Groups[parent]; group != nil && shard != "" {
		for _, child := range group.Children {
			if child.Value.ShardID == shard {
				return fmt.Sprintf("%d-%d", parent, child.Number)
			}
		}
	}
	return ""
}
func (p *Prepared) convertHistory(ctx context.Context, c *taskCatalogue, repo *task.Repository) (int, error) {
	count := 0
	var pending []task.LegacyHistory
	var pendingBytes int
	flush := func() error {
		if err := repo.PutLegacyHistory(p.Manifest.MigrationID, pending); err != nil {
			return err
		}
		pending = nil
		pendingBytes = 0
		return nil
	}
	for _, source := range p.Manifest.Sources {
		err := p.Iterate(ctx, source.Node, func(key, value []byte) error {
			k := string(key)
			var kind string
			var parentIDs []int64
			var taskID int64
			var shard string
			switch {
			case strings.HasPrefix(k, "file-report:") || strings.HasPrefix(k, "completed:"):
				parts := strings.SplitN(k, ":", 4)
				if len(parts) < 3 {
					return errors.New("invalid legacy object ledger key")
				}
				reportID, err := parseOldID(parts[1])
				if err != nil {
					return err
				}
				parent := c.resolveParent(source.Node, reportID)
				var fields struct {
					ParentTaskID *int64 `json:"parentTaskId"`
					TaskID       int64  `json:"taskId"`
					ShardID      string `json:"shardId"`
				}
				// Scan/coverage entries can be booleans or counters; only object envelopes
				// carry execution identity. No absent fields become synthetic successes.
				if parts[0] == "completed" || parts[2] == "item" {
					if json.Unmarshal(value, &fields) != nil {
						return errors.New("malformed legacy object ledger")
					}
					taskID, shard = fields.TaskID, fields.ShardID
					if fields.ParentTaskID != nil && *fields.ParentTaskID > 0 {
						mapped := c.resolveParent(source.Node, *fields.ParentTaskID)
						if mapped == 0 {
							return errors.New("object ledger refers to an absent parent")
						}
						if parent != 0 && parent != mapped {
							return errors.New("object ledger parent identity conflict")
						}
						parent = mapped
					}
				}
				if parent == 0 {
					return errors.New("object ledger has no retained task or purge identity")
				}
				parentIDs = []int64{parent}
				kind = "COVERAGE"
				if parts[0] == "completed" {
					kind = "COMPLETION"
				} else if parts[2] == "item" {
					kind = "OBJECT"
				}
			case strings.HasPrefix(k, "inventory:"):
				parts := strings.SplitN(strings.TrimPrefix(k, "inventory:"), ":", 2)
				if len(parts) != 2 {
					return errors.New("invalid inventory key")
				}
				parentIDs = c.Inventories[source.Node+"\x00"+parts[0]]
				kind = "INVENTORY"
			case strings.HasPrefix(k, "task_creation:"):
				if parent := c.Creations[strings.TrimPrefix(k, "task_creation:")]; parent != 0 {
					parentIDs = []int64{parent}
					kind = "DISCOVERY"
				}
			case strings.HasPrefix(k, "local:explore:") || strings.HasPrefix(k, "local:discovery:") || strings.HasPrefix(k, "local:stop:"):
				parts := strings.Split(k, ":")
				id, err := parseOldID(parts[len(parts)-1])
				if err != nil {
					return err
				}
				if parent := c.resolveParent(source.Node, id); parent != 0 {
					parentIDs = []int64{parent}
					kind = "DISCOVERY"
					if parts[1] == "stop" {
						kind = "FENCE"
					}
				}
			case strings.HasPrefix(k, "failed:") || strings.HasPrefix(k, "fallback:"):
				if strings.HasSuffix(k, ":count") {
					return nil
				}
				var fields struct {
					TaskIndex    int64
					ParentTaskID *int64 `json:"parentTaskId"`
					ShardID      string `json:"shardId"`
				}
				if json.Unmarshal(value, &fields) != nil {
					return errors.New("invalid legacy exception")
				}
				taskID, shard = fields.TaskIndex, fields.ShardID
				parent := c.resolveParent(source.Node, fields.TaskIndex)
				if fields.ParentTaskID != nil {
					if mapped := c.resolveParent(source.Node, *fields.ParentTaskID); mapped != 0 {
						parent = mapped
					}
				}
				if parent != 0 {
					parentIDs = []int64{parent}
					kind = "EXCEPTION"
				}
			case strings.HasPrefix(k, "task_bandwidth:desired:"):
				parentIDs = c.ScopedParents[strings.TrimPrefix(k, "task_bandwidth:desired:")]
				kind = "BANDWIDTH"
			}
			// All remaining records, including allocation counters, pending assignment
			// maps and unused inventories, remain indexed in legacy-source-records and
			// byte-for-byte in the private raw export. They are not executable inputs.
			for _, id := range parentIDs {
				pending = append(pending, task.LegacyHistory{ParentID: id, ChildTaskID: c.childID(source.Node, id, taskID, shard), Node: source.Node, Key: k, Kind: kind, Raw: append([]byte(nil), value...), Cursor: rawRecordKey(source.Node, k)})
				count++
				pendingBytes += len(value) + len(key)
				if len(pending) == 128 || pendingBytes >= 8<<20 {
					if err := flush(); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return count, err
		}
	}
	if err := flush(); err != nil {
		return count, err
	}
	// A single old bandwidth scope must remain a single logical parent. Splitting
	// it would grant each new parent the full cluster cap.
	for scope, ids := range c.ScopedParents {
		if len(ids) > 1 {
			return count, fmt.Errorf("legacy shared bandwidth scope %s spans %s parents; scope reconciliation required", scope, strconv.Itoa(len(ids)))
		}
	}
	return count, nil
}
