package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// legacyRetainedComparisons is the fixed per-node history of Agents before
// Protocol 2.
const legacyRetainedComparisons = 10

type Manager struct {
	mu      sync.Mutex
	db      *state.Store
	factory execution.StoreFactory
	wrap    ReaderWrapper
	options Options
	ctx     context.Context
	cancel  context.CancelFunc
	active  string
	stop    context.CancelFunc
	workers sync.WaitGroup
}

func New(db *state.Store, factory execution.StoreFactory, wrap ReaderWrapper, options Options) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{db: db, factory: factory, wrap: wrap, options: options, ctx: ctx, cancel: cancel}
	err := db.Update(func(tx *state.Tx) error {
		var id string
		if _, err := tx.Get("comparison_runtime", "active", &id); err != nil {
			return err
		}
		if id == "" {
			return nil
		}
		var p Progress
		found, err := tx.Get("comparisons", id, &p)
		if err != nil {
			return err
		}
		if found && (p.Status == "RUNNING" || p.Status == "CANCELING") {
			p.Status = "INTERRUPTED"
			p.UpdatedAt = time.Now().UTC()
			if err := tx.Put("comparisons", id, p); err != nil {
				return err
			}
		}
		return tx.Delete("comparison_runtime", "active")
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return m, nil
}

func (m *Manager) Close() { m.mu.Lock(); m.cancel(); m.mu.Unlock(); m.workers.Wait() }

func (m *Manager) Start(r Request, factories ...execution.StoreFactory) (Progress, error) {
	if err := Validate(r); err != nil {
		return Progress{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Progress{}, err
	}
	var p Progress
	var found bool
	if err := m.db.View(func(tx *state.Tx) error { var err error; found, err = tx.Get("comparisons", r.ID, &p); return err }); err != nil {
		return p, err
	}
	if found {
		if p.Request != r {
			return p, errors.New("comparison ID already used")
		}
		return p, nil
	}
	if m.active != "" {
		return p, ErrBusy
	}
	p = Progress{Request: r, Status: "RUNNING", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), ResultLimit: LegacyStoredDifferences}
	if err := m.db.Update(func(tx *state.Tx) error {
		var history []Progress
		if err := tx.Scan("comparisons", "", "", legacyRetainedComparisons+1, func(_ string, raw json.RawMessage) error {
			var old Progress
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			history = append(history, old)
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(history, func(i, j int) bool { return history[i].StartedAt.Before(history[j].StartedAt) })
		for len(history) >= legacyRetainedComparisons {
			if err := deleteResults(tx, history[0].ID); err != nil {
				return err
			}
			history = history[1:]
		}
		if err := tx.Put("comparisons", r.ID, p); err != nil {
			return err
		}
		return tx.Put("comparison_runtime", "active", r.ID)
	}); err != nil {
		return p, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.active, m.stop = r.ID, cancel
	m.workers.Add(1)
	factory := m.factory
	if len(factories) > 0 {
		factory = factories[0]
	}
	go m.run(ctx, p, factory)
	return p, nil
}

func (m *Manager) run(ctx context.Context, p Progress, factory execution.StoreFactory) {
	defer m.workers.Done()
	rows := make([]Row, 0, 128)
	last := time.Now()
	flush := func() error {
		p.UpdatedAt = time.Now().UTC()
		err := m.db.Update(func(tx *state.Tx) error {
			for _, r := range rows {
				if err := tx.Put("comparison_rows", fmt.Sprintf("%s/%020d", p.ID, r.Number), r); err != nil {
					return err
				}
			}
			if err := tx.Put("comparisons", p.ID, p); err != nil {
				return err
			}
			if p.Status != "RUNNING" && p.Status != "CANCELING" {
				return tx.Delete("comparison_runtime", "active")
			}
			return nil
		})
		if err == nil {
			rows = rows[:0]
			last = time.Now()
		}
		return err
	}
	source, dest, err := factory.Open(ctx, execution.Job{Source: p.Source, Destination: p.Destination})
	if err == nil {
		err = Run(ctx, p.Request, source, dest, m.wrap, RunOptions{}, func(row Row) error {
			p.Compared++
			switch row.Status {
			case "MATCH":
				p.Matched++
			case "DIFFERENT":
				p.Different++
			case "MISSING_TARGET":
				p.Missing++
			case "EXTRA_TARGET":
				p.Extra++
			case "CHANGED_DURING_COMPARE":
				p.Changed++
			default:
				p.Errors++
			}
			if row.Status != "MATCH" {
				if p.Rows < LegacyStoredDifferences {
					p.Rows++
					row.Number = p.Rows
					rows = append(rows, row)
				} else {
					p.RowsTruncated = true
				}
			}
			if len(rows) >= 128 || time.Since(last) >= time.Second {
				return flush()
			}
			return nil
		})
		// A terminal result also promises that this comparison no longer owns
		// storage resources. Keep its admission slot until both ends close.
		err = errors.Join(err, dest.Close(), source.Close())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		err = ctx.Err()
	}
	p.Status = "COMPLETED"
	if errors.Is(err, context.Canceled) {
		p.Status = "CANCELED"
		if m.ctx.Err() != nil {
			p.Status = "INTERRUPTED"
		}
	} else if err != nil {
		p.Status = "FAILED"
		p.Error = "Comparison could not finish; check storage access and retry."
	}
	if m.stop != nil {
		m.stop()
	}
	m.active, m.stop = "", nil
	// Start/Delete share this lock. Once Get can observe the terminal commit,
	// either operation can proceed without racing deferred slot cleanup.
	if e := flush(); e != nil {
		// A failed durable write cannot be reported as a successful comparison.
		p.Status = "FAILED"
		p.Error = "Cannot save comparison results"
		_ = m.db.Update(func(tx *state.Tx) error {
			if err := tx.Put("comparisons", p.ID, p); err != nil {
				return err
			}
			return tx.Delete("comparison_runtime", "active")
		})
	}
}

func deleteResults(tx *state.Tx, id string) error {
	for {
		var keys []string
		if err := tx.Scan("comparison_rows", id+"/", "", 256, func(key string, _ json.RawMessage) error { keys = append(keys, key); return nil }); err != nil {
			return err
		}
		for _, key := range keys {
			if err := tx.Delete("comparison_rows", key); err != nil {
				return err
			}
		}
		if len(keys) < 256 {
			break
		}
	}
	return tx.Delete("comparisons", id)
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == id {
		return ErrBusy
	}
	return m.db.Update(func(tx *state.Tx) error { return deleteResults(tx, id) })
}

func (m *Manager) Cancel(id string) (Progress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.Get(id, 0, 1)
	if err != nil {
		return Progress{}, err
	}
	if m.active == id && m.stop != nil {
		m.stop()
		p.Status = "CANCELING"
	}
	return p.Progress, nil
}

func (m *Manager) Get(id string, after int64, limit int) (Page, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return Page{}, errors.New("invalid result pagination")
	}
	result := Page{Items: []Row{}}
	err := m.db.View(func(tx *state.Tx) error {
		found, err := tx.Get("comparisons", id, &result.Progress)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return tx.Scan("comparison_rows", id+"/", fmt.Sprintf("%s/%020d", id, after), limit, func(_ string, raw json.RawMessage) error {
			var row Row
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			result.Items = append(result.Items, row)
			return nil
		})
	})
	if len(result.Items) > 0 && result.Items[len(result.Items)-1].Number < result.Rows {
		result.NextAfter = result.Items[len(result.Items)-1].Number
	}
	return result, err
}

// Rows serves comparison_rows: up to q.Limit rows after q.After, optionally
// filtered by status. See Page for the cursor semantics.
func (m *Manager) Rows(q RowsQuery) (Page, error) {
	return Page{}, errors.New("comparison rows are not implemented")
}

// List returns every retained comparison, newest first, without rows.
func (m *Manager) List() ([]Progress, error) {
	return nil, errors.New("comparison list is not implemented")
}

// Resume continues an interrupted, failed or canceled prefix comparison from
// its checkpoint with factory. It returns ErrNotResumable when the record has
// no usable checkpoint, ErrBusy when another comparison runs and ErrNotFound
// when it does not exist.
func (m *Manager) Resume(id string, factory execution.StoreFactory) (Progress, error) {
	return Progress{}, errors.New("comparison resume is not implemented")
}
