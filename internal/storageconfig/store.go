package storageconfig

import (
	"errors"
	"math"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const bucket = "storage-configuration"

// An exact replay has no writes to persist. Returning this from the ordinary
// bbolt transaction rolls it back without another metadata-page fsync.
var errSnapshotUnchanged = errors.New("storage snapshot is unchanged")

// Master owns the authoritative snapshot. Agent stores its received snapshot in
// a different record so accidentally constructing both cannot overwrite either.
type Master struct{ store *state.Store }
type Agent struct{ store *state.Store }

func NewMaster(store *state.Store) *Master { return &Master{store: store} }
func NewAgent(store *state.Store) *Agent   { return &Agent{store: store} }

func read(tx *state.Tx, key string) (Snapshot, error) {
	var snapshot Snapshot
	if _, err := tx.Get(bucket, key, &snapshot); err != nil {
		return Snapshot{}, err
	}
	return canonical(snapshot)
}

func snapshot(store *state.Store, key string) (Snapshot, error) {
	var result Snapshot
	err := store.View(func(tx *state.Tx) error {
		var err error
		result, err = read(tx, key)
		return err
	})
	return result, err
}

// MasterSnapshotTx reads the authoritative configuration inside another durable transaction.
func MasterSnapshotTx(tx *state.Tx) (Snapshot, error) { return read(tx, "master") }

func (m *Master) Snapshot() (Snapshot, error) { return snapshot(m.store, "master") }
func (a *Agent) Snapshot() (Snapshot, error)  { return snapshot(a.store, "agent") }

func (m *Master) mutate(change func(*Snapshot) error) (Snapshot, error) {
	var result Snapshot
	err := m.store.Update(func(tx *state.Tx) error {
		current, err := read(tx, "master")
		if err != nil {
			return err
		}
		if current.Revision == math.MaxUint64 {
			return ErrRevisionLimit
		}
		if err := change(&current); err != nil {
			return err
		}
		current.Revision++
		result, err = canonical(current)
		if err != nil {
			return err
		}
		return tx.Put(bucket, "master", result)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

func (m *Master) Create(config Config) (Snapshot, error) {
	if err := Validate(config); err != nil {
		return Snapshot{}, err
	}
	config = cloneConfig(config)
	return m.mutate(func(current *Snapshot) error {
		for _, existing := range current.Connections {
			if existing.Alias == config.Alias {
				return ErrExists
			}
		}
		current.Connections = append(current.Connections, config)
		return nil
	})
}

// Update replaces one connection. Renaming requires an explicit delete/create;
// an endpoint referring to the old alias must never silently change identity.
func (m *Master) Update(alias string, config Config) (Snapshot, error) {
	if !validAlias(alias) || config.Alias != alias {
		return Snapshot{}, invalid("update alias must match the existing alias")
	}
	if err := Validate(config); err != nil {
		return Snapshot{}, err
	}
	config = cloneConfig(config)
	return m.mutate(func(current *Snapshot) error {
		for index := range current.Connections {
			if current.Connections[index].Alias == alias {
				current.Connections[index] = config
				return nil
			}
		}
		return ErrNotFound
	})
}

func (m *Master) Delete(alias string) (Snapshot, error) {
	if !validAlias(alias) {
		return Snapshot{}, invalid("alias")
	}
	return m.mutate(func(current *Snapshot) error {
		for index := range current.Connections {
			if current.Connections[index].Alias == alias {
				current.Connections = append(current.Connections[:index], current.Connections[index+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// ApplySnapshot replaces the entire replica atomically. applied=false with no
// error means an exact retry; an older revision is rejected even after restart.
func (a *Agent) ApplySnapshot(incoming Snapshot) (applied bool, err error) {
	next, err := canonical(incoming)
	if err != nil {
		return false, err
	}
	err = a.store.Update(func(tx *state.Tx) error {
		current, err := read(tx, "agent")
		if err != nil {
			return err
		}
		if next.Revision < current.Revision {
			return ErrStaleRevision
		}
		if next.Revision == current.Revision {
			if !reflect.DeepEqual(next.Connections, current.Connections) {
				return ErrRevisionConflict
			}
			return errSnapshotUnchanged
		}
		if err := tx.Put(bucket, "agent", next); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err == errSnapshotUnchanged {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return applied, nil
}
