package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestReserveIDMonotonicAndTransactional(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	abort := errors.New("abort")
	if err := db.Update(func(tx *Tx) error {
		if err := tx.ReserveID("ids", 10000); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error {
		next, err := tx.NextID("ids")
		if err != nil {
			return err
		}
		if next != 1 {
			t.Fatal(next)
		}
		if err := tx.ReserveID("ids", 100); err != nil {
			return err
		}
		if err := tx.ReserveID("ids", 10); err != nil {
			return err
		}
		next, err = tx.NextID("ids")
		if next != 101 {
			t.Fatal(next)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
