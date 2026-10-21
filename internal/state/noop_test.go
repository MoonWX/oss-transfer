package state

import (
	"errors"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func transactionID(t *testing.T, s *Store) int {
	t.Helper()
	var id int
	if err := s.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUnchangedUpdatesDoNotWriteDatabasePages(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(tx *Tx) error {
		if err := tx.ReserveID("records", 17); err != nil {
			return err
		}
		return tx.Put("records", "task", map[string]int{"completed": 128})
	}); err != nil {
		t.Fatal(err)
	}
	id, before := transactionID(t, s), s.db.Stats()
	calls := 0
	for range 100 {
		if err := s.Update(func(tx *Tx) error {
			calls++
			if err := tx.Put("records", "task", map[string]int{"completed": 128}); err != nil {
				return err
			}
			if err := tx.Delete("records", "missing"); err != nil {
				return err
			}
			return tx.ReserveID("records", 17)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Update(func(tx *Tx) error { var value any; _, err := tx.Get("records", "task", &value); return err }); err != nil {
		t.Fatal(err)
	}
	after := s.db.Stats()
	if calls != 100 || transactionID(t, s) != id || after.TxStats.GetWrite() != before.TxStats.GetWrite() {
		t.Fatalf("unchanged updates wrote pages or replayed callbacks: calls=%d writes=%d", calls, after.TxStats.GetWrite()-before.TxStats.GetWrite())
	}
}

func TestNoopOptimizationPreservesTransactionsAndSequences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *Tx) error {
		id, err := tx.NextID("ids")
		if id != 1 {
			t.Fatalf("first ID: %d", id)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *Tx) error { return tx.ReserveID("empty", 0) }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("rollback")
	if err := s.Update(func(tx *Tx) error {
		if _, err := tx.NextID("ids"); err != nil {
			return err
		}
		if err := tx.Put("records", "key", "new"); err != nil {
			return err
		}
		var value string
		if found, err := tx.Get("records", "key", &value); err != nil || !found || value != "new" {
			t.Fatalf("read own write: %v %q %v", found, value, err)
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(tx *Tx) error {
		if tx.tx.Bucket([]byte("empty")) == nil {
			t.Fatal("empty bucket creation was lost")
		}
		var value any
		if found, err := tx.Get("records", "key", &value); found || err != nil {
			t.Fatalf("failed write survived: %v %v", found, err)
		}
		id, err := tx.NextID("ids")
		if id != 2 {
			t.Fatalf("allocator lost durable ID or retained rolled-back ID: %d", id)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNoopOptimizationPreservesWriteErrors(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(tx *Tx) error {
		if err := tx.Put("records", "key", "value"); err != nil {
			return err
		}
		_, err := tx.tx.Bucket([]byte("records")).CreateBucket([]byte("nested"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func(*Tx) error{
		func(tx *Tx) error { return tx.Put("records", "key", "value") },
		func(tx *Tx) error { return tx.Delete("records", "missing") },
		func(tx *Tx) error { return tx.ReserveID("records", 0) },
	} {
		if err := s.View(call); !errors.Is(err, bolt.ErrTxNotWritable) {
			t.Fatalf("read-only write was accepted: %v", err)
		}
	}
	if err := s.Update(func(tx *Tx) error { return tx.Delete("records", "nested") }); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Fatalf("nested bucket deletion error changed: %v", err)
	}
	if err := s.Update(func(tx *Tx) error {
		if err := tx.Put("created", "", true); !errors.Is(err, bolt.ErrKeyRequired) {
			t.Fatalf("empty key: %v", err)
		}
		return nil // A caller may handle the failed Put; bucket creation still commits.
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(tx *Tx) error {
		if tx.tx.Bucket([]byte("created")) == nil {
			t.Fatal("handled Put error lost bucket creation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
