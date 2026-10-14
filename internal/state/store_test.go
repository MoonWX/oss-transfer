package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestDurableAtomicTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("disk operation failed")
	if err := s.Update(func(tx *Tx) error {
		if err := tx.Put("tasks", "1", map[string]any{"stopped": true}); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.View(func(tx *Tx) error {
		var v any
		found, err := tx.Get("tasks", "1", &v)
		if found {
			t.Error("failed transaction became visible")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *Tx) error {
		if err := tx.Put("tasks", "1", map[string]any{"stopped": true}); err != nil {
			return err
		}
		return tx.Put("results", "1/file", map[string]any{"bytes": 123})
	}); err != nil {
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
	if err := s.View(func(tx *Tx) error {
		var task struct{ Stopped bool }
		found, err := tx.Get("tasks", "1", &task)
		if !found || !task.Stopped {
			t.Error("committed stop fence missing after reopen")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentIDsAndBoundedPagination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(tx *Tx) error {
				id, err := tx.NextID("items")
				if err != nil {
					return err
				}
				return tx.Put("items", fmt.Sprintf("task1/%04d", id), id)
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var keys []string
	after := ""
	for {
		var page []string
		err := s.View(func(tx *Tx) error {
			return tx.Scan("items", "task1/", after, 7, func(k string, v json.RawMessage) error { page = append(page, k); return nil })
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 7 {
			t.Fatal("unbounded page")
		}
		keys = append(keys, page...)
		after = page[len(page)-1]
	}
	var want []string
	for i := 1; i <= 40; i++ {
		want = append(want, fmt.Sprintf("task1/%04d", i))
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("lost or duplicate records: %v", keys)
	}
}

func TestRefuseLegacyDatabaseWithoutCreatingFiles(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rocksdb")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "CURRENT"), []byte("MANIFEST-000001\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "master.db")); !errors.Is(err, ErrLegacyDatabase) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "master.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created a fresh database next to legacy data")
	}
	if _, err := Open(filepath.Join(dir, "master", "master.db")); !errors.Is(err, ErrLegacyDatabase) {
		t.Fatal("default Go path did not detect sibling V4 database", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "master")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created the Go data directory before refusing legacy state")
	}
}
