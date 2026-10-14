package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func descriptor(name, content string) Descriptor {
	return Descriptor{ID: digestText(name), Name: name, ContentSHA256: digestText(content), Binding: digestText("binding"), SpecSHA256: digestText("spec")}
}
func openRepo(t *testing.T, path string) (*Repository, *state.Store) {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return New(db), db
}
func TestUploadSurvivesRestartDeduplicatesAndRejectsReplayChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	r, db := openRepo(t, path)
	d := descriptor("names.incr", " spaced \na\n spaced \nb\n")
	if _, err := r.Begin(ctx, d); err != nil {
		t.Fatal(err)
	}
	first := Batch{ID: d.ID, Number: 1, Keys: []string{" spaced ", "a"}}
	if _, err := r.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, db = openRepo(t, path)
	defer db.Close()
	if h, err := r.Begin(ctx, d); err != nil || h.NextBatch != 2 {
		t.Fatal(h, err)
	}
	if h, err := r.Put(ctx, first); err != nil || h.RawCount != 2 {
		t.Fatal(h, err)
	}
	wrong := first
	wrong.Keys = []string{"changed"}
	if _, err := r.Put(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := r.Seal(ctx, Seal{d.ID, 2, 3}); !errors.Is(err, ErrConflict) {
		t.Fatal("truncated upload sealed", err)
	}
	if _, _, err := r.Keys(ctx, d.ID, "", 128); !errors.Is(err, ErrUnsealed) {
		t.Fatal(err)
	}
	if _, err := r.Put(ctx, Batch{d.ID, 2, []string{" spaced ", "b"}}); err != nil {
		t.Fatal(err)
	}
	if h, err := r.Seal(ctx, Seal{d.ID, 2, 3}); err != nil || h.RawCount != 4 || h.UniqueCount != 3 {
		t.Fatal(h, err)
	}
	if _, err := r.Put(ctx, first); err != nil {
		t.Fatal("sealed same-batch retry", err)
	}
	if _, err := r.Put(ctx, Batch{d.ID, 3, []string{"extra"}}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	keys, _, err := r.Keys(ctx, d.ID, "", 128)
	if err != nil || len(keys) != 3 || !strings.Contains(strings.Join(keys, "|"), " spaced ") {
		t.Fatal(keys, err)
	}
	d.Binding = digestText("new enrollment")
	if _, err := r.Begin(ctx, d); !errors.Is(err, ErrConflict) {
		t.Fatal("binding changed", err)
	}
}
func TestConcurrentBatchAndLimitRollback(t *testing.T) {
	ctx := context.Background()
	r, db := openRepo(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	d := descriptor("concurrent.incr", "a\n")
	_, _ = r.Begin(ctx, d)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Put(ctx, Batch{d.ID, 1, []string{"a"}}); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	h, _ := r.Get(ctx, d.ID)
	if h.RawCount != 1 || h.UniqueCount != 1 {
		t.Fatal(h)
	}
	if err := db.Update(func(tx *state.Tx) error {
		v, e := read(tx, d.ID)
		if e != nil {
			return e
		}
		v.RawCount = MaxRawKeys - 1
		return tx.Put("inventory-manifests", d.ID, v)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Put(ctx, Batch{d.ID, 2, []string{"new1", "new2"}}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	h, _ = r.Get(ctx, d.ID)
	if h.RawCount != MaxRawKeys-1 || h.UniqueCount != 1 || h.NextBatch != 2 {
		t.Fatal("partial commit", h)
	}
	count := 0
	_ = db.View(func(tx *state.Tx) error {
		return tx.Scan("inventory-keys", d.ID+"/", "", 128, func(_ string, _ json.RawMessage) error { count++; return nil })
	})
	if count != 1 {
		t.Fatal("failed batch leaked key", count)
	}
}
func TestCancelReleasesUploadCapacityAndCannotRevive(t *testing.T) {
	ctx := context.Background()
	r, db := openRepo(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	var first Descriptor
	for i := range MaxUploading {
		d := descriptor(fmt.Sprintf("%d.incr", i), "key\n")
		if i == 0 {
			first = d
		}
		if _, err := r.Begin(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	extra := descriptor("extra.incr", "")
	if _, err := r.Begin(ctx, extra); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if _, err := r.Put(ctx, Batch{first.ID, 1, []string{"key"}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if h, err := r.Cancel(ctx, first.ID); err != nil || h.State != "CANCELLED" {
			t.Fatal(h, err)
		}
	}
	if _, err := r.Begin(ctx, extra); err != nil {
		t.Fatal("capacity was not released", err)
	}
	if _, err := r.Seal(ctx, Seal{first.ID, 1, 1}); !errors.Is(err, ErrConflict) {
		t.Fatal("cancelled upload revived", err)
	}
	if _, err := r.Put(ctx, Batch{first.ID, 1, []string{"key"}}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := r.Batch(ctx, first.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancel left payload", err)
	}
}
func TestSealChecksContentAndEmptyManifest(t *testing.T) {
	ctx := context.Background()
	r, db := openRepo(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	d := descriptor("wrong.incr", "right\n")
	_, _ = r.Begin(ctx, d)
	_, _ = r.Put(ctx, Batch{d.ID, 1, []string{"wrong"}})
	if _, err := r.Seal(ctx, Seal{d.ID, 1, 1}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	d = descriptor("empty.incr", "")
	_, _ = r.Begin(ctx, d)
	if _, err := r.Seal(ctx, Seal{d.ID, 0, 0}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", strings.Repeat("x", 1025), "a\x00b", "a\nb", string([]byte{0xff})} {
		if err := ValidateKey(key); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
