package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// LegacyProvenance identifies a historical observation, not a verified Go
// execution result. Raw V4 records stay separate from the execution ledger.
type LegacyProvenance struct {
	RecoveryReady       bool     `json:"recoveryReady,omitempty"`
	RecoveryStarted     bool     `json:"recoveryStarted,omitempty"`
	MigrationID         string   `json:"migrationId"`
	OriginalStatus      string   `json:"originalStatus"`
	NeedsReconciliation bool     `json:"needsReconciliation"`
	Blockers            []string `json:"blockers,omitempty"`
	SourceNodes         []string `json:"sourceNodes"`
	ScopeID             string   `json:"scopeId,omitempty"`
	ImportHash          string   `json:"importHash"`
}

// LegacyChild retains the node-local identity and complete original record.
// Number is only its established public child number, starting at one.
// This type deliberately has no execution.Job or successful Target fingerprint.
type LegacyChild struct {
	ParentID int64           `json:"parentId"`
	Number   int             `json:"number"`
	Node     string          `json:"node"`
	TaskID   int64           `json:"legacyTaskId"`
	Key      string          `json:"legacyKey"`
	Raw      json.RawMessage `json:"raw"`
}

type LegacyParentImport struct {
	Parent   Parent        `json:"parent"`
	Children []LegacyChild `json:"children"`
}

type LegacyImport struct {
	MigrationID string
	HighWater   uint64
	Parents     []LegacyParentImport
	PurgedIDs   []int64
}

func requireMigration(tx *state.Tx, migrationID string) error {
	var record struct {
		MigrationID string `json:"migrationId"`
		Pending     *bool  `json:"pending"`
	}
	found, err := tx.Get("migration", "state", &record)
	if err != nil {
		return err
	}
	if migrationID == "" || !found || record.MigrationID != migrationID || record.Pending == nil || !*record.Pending {
		return errors.New("legacy import requires the matching pending migration")
	}
	return nil
}

// ImportLegacy is an offline-only atomic catalogue import. IDs, tombstones,
// children and listing indexes share one commit. No runnable work is created.
// Replaying identical input is safe; changing an already imported parent fails.
func (r *Repository) ImportLegacy(input LegacyImport) error {
	if len(input.Parents) > 100000 || len(input.PurgedIDs) > 100000 || input.HighWater > 1<<53-2 {
		return errors.New("invalid legacy import bounds")
	}
	return r.store.Update(func(tx *state.Tx) error {
		if err := requireMigration(tx, input.MigrationID); err != nil {
			return err
		}
		high := input.HighWater
		seen := map[int64]bool{}
		purged := map[int64]bool{}
		for _, id := range input.PurgedIDs {
			if id <= 0 || id > 1<<53-2 {
				return errors.New("invalid legacy purge identity")
			}
			purged[id] = true
			high = max(high, uint64(id))
		}
		for _, row := range input.Parents {
			p := row.Parent
			if p.ID <= 0 || p.ID > 1<<53-2 || seen[p.ID] || p.Legacy == nil || p.Legacy.MigrationID != input.MigrationID || !Ended(p.Status) || p.Generation != 1 || p.BudgetRevision != 1 || p.ChildCount != len(row.Children) || len(row.Children) > 100000 || p.DispatchedThrough != 0 || p.ScannedBatches != 0 || p.Legacy.ImportHash != "" {
				return errors.New("invalid legacy parent")
			}
			if p.Status != "COMPLETED" && !p.Legacy.NeedsReconciliation {
				return errors.New("unfinished legacy task must require reconciliation")
			}
			seen[p.ID] = true
			high = max(high, uint64(p.ID))
			if purged[p.ID] {
				p.Purged, p.Deleted = true, true
			}
			if p.Purged && !p.Deleted {
				return errors.New("purged legacy parent must remain deleted")
			}
			importHash := digest(row)
			existing, err := get(tx, p.ID)
			if err == nil {
				if existing.Legacy == nil || existing.Legacy.MigrationID != input.MigrationID || existing.Legacy.ImportHash != importHash || existing.Purged != p.Purged {
					return ErrConflict
				}
				continue
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			numbers := map[int]bool{}
			for _, child := range row.Children {
				if child.ParentID != p.ID || child.Number < 1 || child.Number > 1<<31 || numbers[child.Number] || child.Node == "" || child.Key == "" || len(child.Raw) == 0 || !json.Valid(child.Raw) || len(child.Raw) > 64<<20 {
					return errors.New("invalid or ambiguous legacy child")
				}
				numbers[child.Number] = true
				if !p.Purged {
					if err := tx.Put("master-task-legacy-children", childKey(p.ID, child.Number), child); err != nil {
						return err
					}
				}
			}
			if !p.Purged {
				ordered := make([]int, 0, len(numbers))
				for number := range numbers {
					ordered = append(ordered, number)
				}
				sort.Ints(ordered)
				for offset, number := range ordered {
					if err := tx.Put("master-task-legacy-order", childKey(p.ID, offset+1), number); err != nil {
						return err
					}
				}
			}
			provenance := *p.Legacy
			provenance.ImportHash = importHash
			p.Legacy = &provenance
			p.Fingerprint = digest(p.Spec)
			if err := save(tx, p); err != nil {
				return err
			}
		}
		for id := range purged {
			if seen[id] {
				continue
			}
			p, err := get(tx, id)
			if err == nil {
				if !p.Purged || p.Legacy == nil || p.Legacy.MigrationID != input.MigrationID {
					return ErrConflict
				}
				continue
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			p = Parent{ID: id, Generation: 1, BudgetRevision: 1, Status: "STOPPED", Deleted: true, Purged: true, Legacy: &LegacyProvenance{MigrationID: input.MigrationID, OriginalStatus: "PURGED", NeedsReconciliation: true}}
			if err := save(tx, p); err != nil {
				return err
			}
		}
		return tx.ReserveID("master-tasks", high)
	})
}

func (r *Repository) LegacyChildren(id int64, after, limit int) (rows []LegacyChild, err error) {
	if after < 0 || limit < 1 || limit > 256 {
		return nil, errors.New("invalid legacy child page")
	}
	err = r.store.View(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Purged {
			return ErrNotFound
		}
		return tx.Scan("master-task-legacy-children", key(id)+"/", childKey(id, after), limit, func(_ string, raw json.RawMessage) error {
			var child LegacyChild
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			rows = append(rows, child)
			return nil
		})
	})
	return
}

// LegacyHistory is a typed envelope around a retained V4 record. Cursor orders
// records by node and original key; it is independent of the object's name.
type LegacyHistory struct {
	ParentID    int64           `json:"parentId"`
	ChildTaskID string          `json:"childTaskId,omitempty"`
	Node        string          `json:"node"`
	Key         string          `json:"key"`
	Kind        string          `json:"kind"`
	Raw         json.RawMessage `json:"raw"`
	Cursor      string          `json:"cursor"`
}

// PutLegacyHistory commits a bounded page of immutable records. A migration
// section is marked complete separately, after all source pages were consumed.
func (r *Repository) PutLegacyHistory(migrationID string, rows []LegacyHistory) error {
	if len(rows) > 128 {
		return errors.New("legacy history batch exceeds 128 records")
	}
	return r.store.Update(func(tx *state.Tx) error {
		if err := requireMigration(tx, migrationID); err != nil {
			return err
		}
		for _, row := range rows {
			if row.ParentID < 1 || row.Node == "" || row.Key == "" || row.Cursor == "" || strings.ContainsAny(row.Cursor, "\x00\r\n") || len(row.Cursor) > 8192 || !json.Valid(row.Raw) || len(row.Raw) > 64<<20 {
				return errors.New("invalid legacy history record")
			}
			switch row.Kind {
			case "OBJECT", "COMPLETION", "COVERAGE", "DISCOVERY", "INVENTORY", "EXCEPTION", "BANDWIDTH", "FENCE":
			default:
				return errors.New("invalid legacy history kind")
			}
			p, err := get(tx, row.ParentID)
			if err != nil {
				return err
			}
			if p.Legacy == nil || p.Legacy.MigrationID != migrationID {
				return ErrConflict
			}
			if p.Purged {
				// Source raw snapshots still retain deletion evidence. Never revive
				// a purged public report by indexing stale worker history.
				continue
			}
			k := fmt.Sprintf("%s/%s/%s", key(row.ParentID), row.Kind, row.Cursor)
			var existing LegacyHistory
			found, err := tx.Get("master-task-legacy-history", k, &existing)
			if err != nil {
				return err
			}
			if found && digest(existing) != digest(row) {
				return ErrConflict
			}
			if !found {
				if err := tx.Put("master-task-legacy-history", k, row); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (r *Repository) LegacyHistory(id int64, kind, after string, limit int) (rows []LegacyHistory, err error) {
	if limit < 1 || limit > 256 || strings.ContainsAny(kind, "/\x00") || kind == "" || strings.ContainsAny(after, "\x00\r\n") {
		return nil, errors.New("invalid legacy history page")
	}
	err = r.store.View(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Purged {
			return ErrNotFound
		}
		prefix := key(id) + "/" + kind + "/"
		cursor := ""
		if after != "" {
			cursor = prefix + after
		}
		return tx.Scan("master-task-legacy-history", prefix, cursor, limit, func(_ string, raw json.RawMessage) error {
			var row LegacyHistory
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
	})
	return
}

// LegacyChildrenPage uses an ordinal index because old public child numbers can
// contain gaps. An HTTP offset counts rows, not historical child numbers.
func (r *Repository) LegacyChildrenPage(id int64, offset, limit int) (rows []LegacyChild, err error) {
	if offset < 0 || limit < 1 || limit > 256 {
		return nil, errors.New("invalid legacy child page")
	}
	err = r.store.View(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Purged {
			return ErrNotFound
		}
		return tx.Scan("master-task-legacy-order", key(id)+"/", childKey(id, offset), limit, func(_ string, raw json.RawMessage) error {
			var number int
			if err := json.Unmarshal(raw, &number); err != nil {
				return err
			}
			var child LegacyChild
			found, err := tx.Get("master-task-legacy-children", childKey(id, number), &child)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("legacy child index is incomplete")
			}
			rows = append(rows, child)
			return nil
		})
	})
	return
}
