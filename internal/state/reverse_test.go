package state

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReverseScanExclusiveBoundaries(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *Tx) error {
		for _, k := range []string{"01", "03", "05"} {
			if err := tx.Put("rows", k, k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		before string
		limit  int
		want   []string
	}{
		{"", 2, []string{"05", "03"}}, {"05", 5, []string{"03", "01"}},
		{"04", 5, []string{"03", "01"}}, {"99", 5, []string{"05", "03", "01"}},
		{"01", 5, nil}, {"00", 5, nil},
	} {
		var got []string
		err := db.View(func(tx *Tx) error {
			return tx.ScanReverse("rows", tc.before, tc.limit, func(k string, _ json.RawMessage) error { got = append(got, k); return nil })
		})
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("before=%q: %v, %v; want %v", tc.before, got, err, tc.want)
		}
	}
}
