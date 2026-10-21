package state

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

type rejectedEncoding struct{}

var errEncodingRejected = errors.New("injected storage encoding failure")

func (rejectedEncoding) MarshalStateJSON() ([]byte, error) { return nil, errEncodingRejected }

func TestStorageEncodingFailureRollsBackAndNilPointerRemainsNull(t *testing.T) {
	var pointer *rejectedEncoding
	data, err := json.Marshal(ForStorage(pointer))
	if err != nil || string(data) != "null" {
		t.Fatalf("nil pointer changed JSON semantics: %s, %v", data, err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *Tx) error {
		if err := tx.Put("test", "first", "must roll back"); err != nil {
			return err
		}
		return tx.Put("test", "rejected", rejectedEncoding{})
	})
	if !errors.Is(err, errEncodingRejected) {
		t.Fatalf("storage encoding error was lost: %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		var value string
		found, err := tx.Get("test", "first", &value)
		if err == nil && found {
			return errors.New("encoding failure committed preceding writes")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
