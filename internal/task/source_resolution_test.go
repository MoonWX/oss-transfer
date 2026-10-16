package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestObserveInventoryResolutionPreservesIdentityAndCorrectsCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.InventoryID = digest("missing-key-manifest")
	sum := sha256.Sum256([]byte("missing\n"))
	inventories := inventory.New(db)
	descriptor := inventory.Descriptor{ID: spec.InventoryID, Name: "missing.incr", ContentSHA256: hex.EncodeToString(sum[:]), Binding: digest("binding"), SpecSHA256: digest(spec)}
	if _, err := inventories.Begin(context.Background(), descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := inventories.Put(context.Background(), inventory.Batch{ID: spec.InventoryID, Number: 1, Keys: []string{"missing"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := inventories.Seal(context.Background(), inventory.Seal{ID: spec.InventoryID, BatchCount: 1, UniqueCount: 1}); err != nil {
		t.Fatal(err)
	}
	p, _, err := repo.Create(spec, "missing")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.Assign(p.ID, 1, 1, []transferstore.Object{{Key: "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	item := c.Job.Items[0]
	record := execution.Record{Job: c.Job, Attempt: 1, UpdatedAt: time.Now().UTC(), Status: execution.Failed, Results: []execution.ItemResult{{ID: item.ID, Status: execution.ItemFailed, Error: "not found"}}, Counters: execution.Counters{Total: 1, Failed: 1}}
	if err = repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	source := transferstore.Object{Key: "missing", ETag: "first available", Size: 23}
	record.Attempt++
	record.Results[0].ResolvedSource = &source
	record.Results[0].Error = "write failed after resolution"
	record.Counters = execution.Counters{Total: 1, TotalBytes: 23, Failed: 1, FailedBytes: 23}
	if err = repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	var failures []Exception
	if err = repo.VisitExceptions(context.Background(), ExceptionFilter{ParentID: p.ID}, func(e Exception) error { failures = append(failures, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Size != 23 {
		t.Fatalf("exception size stayed zero: %+v", failures)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	record.Attempt++
	record.Results[0].Status = execution.ItemSucceeded
	record.Results[0].Error = ""
	record.Status = execution.Completed
	record.Counters = execution.Counters{Total: 1, TotalBytes: 23, Succeeded: 1, SucceededBytes: 23}
	if err = repo.Observe(p.ID, 1, 1, record); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = repo.Observe(p.ID, 1, 1, record); err != nil {
			t.Fatal(err)
		}
	}
	p, err = repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Counters != record.Counters {
		t.Fatalf("retry/replay counters: %+v", p.Counters)
	}
	changed := source
	changed.ETag = "other version"
	record.Results[0].ResolvedSource = &changed
	record.Attempt++
	if err = repo.Observe(p.ID, 1, 1, record); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin changed after restart: %v", err)
	}
	record.Results[0].ResolvedSource = nil
	if err = repo.Observe(p.ID, 1, 1, record); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin removed after restart: %v", err)
	}
	// An ordinary task cannot claim inventory provenance just by returning a pin.
	ordinary, _, err := repo.Create(testSpec(), "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Assign(ordinary.ID, 1, 1, []transferstore.Object{{Key: "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	record.Job = child.Job
	record.Results[0].ID = child.Job.Items[0].ID
	record.Results[0].ResolvedSource = &source
	if err = repo.Observe(ordinary.ID, 1, 1, record); !errors.Is(err, ErrConflict) {
		t.Fatalf("ordinary source was rebound: %v", err)
	}
}
