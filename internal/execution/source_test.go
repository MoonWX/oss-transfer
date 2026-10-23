package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func samePersistedSourceValue(a, b any) bool {
	// Compare every stored field, without comparing time.Time's in-memory
	// Location or monotonic clock, which JSON does not persist.
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func missingInventoryFixture(t *testing.T) *fixture {
	f := newFixture(t, 2)
	if err := f.db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	// Local store supplies actual durable bytes; the assignment models an S3
	// inventory entry whose initial HEAD returned not found.
	f.job.Source.Kind = "S3"
	item := &f.job.Items[1]
	if err := os.Remove(filepath.Join(f.src, item.Source.Key)); err != nil {
		t.Fatal(err)
	}
	item.Source = transferstore.Object{Key: item.Source.Key}
	return f
}

func TestMissingInventoryRetryPinsFirstIdentityAndCounts(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "new inventory", true: "historical placeholder"}[historical], func(t *testing.T) {
			f := missingInventoryFixture(t)
			f.startEngine(nil, Options{})
			if _, err := f.engine.Accept(bg, f.job, !historical); err != nil {
				t.Fatal(err)
			}
			if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
				t.Fatal(err)
			}
			first := f.wait()
			if first.Status != PartialFailed || first.Counters.Failed != 1 || first.Results[1].ResolvedSource != nil {
				t.Fatalf("initial ledger: %+v", first)
			}
			data := []byte("supplied after discovery")
			if err := os.WriteFile(filepath.Join(f.src, f.job.Items[1].Source.Key), data, 0600); err != nil {
				t.Fatal(err)
			}
			if historical {
				// This is the real Master retry path, including the old Job digest.
				if err := f.engine.StopParent(bg, 224, 1); err != nil {
					t.Fatal(err)
				}
				if err := f.engine.ResumeParent(bg, 224, 2, true, true); err != nil {
					t.Fatal(err)
				}
				f.job.Generation = 2
			}
			if err := f.engine.Resume(bg, f.job.ChildID, f.job.Generation, true); err != nil {
				t.Fatal(err)
			}
			done := f.wait()
			if done.Status != Completed || done.Counters != (Counters{Total: 2, TotalBytes: int64(len(data)), Succeeded: 2, SucceededBytes: int64(len(data))}) {
				t.Fatalf("repaired ledger: %+v", done)
			}
			if done.Results[0].ResolvedSource != nil || done.Results[1].ResolvedSource == nil || !samePersistedSourceValue(done.Job, f.job) || done.Fingerprint != first.Fingerprint {
				t.Fatal("assignment changed or wrong object rebound")
			}
			if err := done.ValidateResolvedSources(true, &first); err != nil {
				t.Fatal(err)
			}
			f.assertFiles()
		})
	}
}

type pinCheckingSource struct {
	transferstore.Store
	repo  *Repository
	child string
	opens *atomic.Int64
	fail  bool
}

func (s *pinCheckingSource) Open(ctx context.Context, key string, opts transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	s.opens.Add(1)
	record, err := s.repo.Get(ctx, s.child)
	if err != nil {
		return nil, transferstore.Object{}, err
	}
	for _, result := range record.Results {
		if result.ResolvedSource != nil && result.ResolvedSource.Key == key {
			if !samePersistedSourceValue(result.ResolvedSource, opts.Expected) {
				return nil, transferstore.Object{}, errors.New("read did not use durable pin")
			}
			if s.fail {
				return nil, transferstore.Object{}, errors.New("injected read failure after pin")
			}
			return s.Store.Open(ctx, key, opts)
		}
	}
	return nil, transferstore.Object{}, errors.New("read before durable pin")
}

func TestResolvedInventoryIdentitySurvivesRestartAndRejectsChangedSource(t *testing.T) {
	f := missingInventoryFixture(t)
	f.job.Items = f.job.Items[1:]
	key := f.job.Items[0].Source.Key
	if err := os.WriteFile(filepath.Join(f.src, key), []byte("first version"), 0600); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int64
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		s, d, err := base.Open(ctx, j)
		return &pinCheckingSource{Store: s, repo: f.repo, child: j.ChildID, opens: &opens, fail: true}, d, err
	}), Options{})
	if _, err := f.engine.Accept(bg, f.job, true); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	first := f.wait()
	if first.Status != Failed || opens.Load() != 1 || first.Results[0].ResolvedSource == nil || !strings.Contains(first.Results[0].Error, "injected read failure after pin") {
		t.Fatalf("pin before read failed: %+v", first)
	}
	if err := f.engine.Close(bg); err != nil {
		t.Fatal(err)
	}
	// Leave a RUNNING attempt durably behind as a process crash would.
	if _, err := f.repo.begin(bg, f.job.ChildID, 1, true, true); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.repo, err = NewRepository(f.db, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.src, key), []byte("other version"), 0600); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	if err := f.engine.Resume(bg, f.job.ChildID, 1, true); err != nil {
		t.Fatal(err)
	}
	after := f.wait()
	if after.Status != Failed || !strings.Contains(after.Results[0].Error, "source changed") || !samePersistedSourceValue(after.Results[0].ResolvedSource, first.Results[0].ResolvedSource) {
		t.Fatalf("source rebound after restart: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(f.dst, f.job.Items[0].DestinationKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed source was published: %v", err)
	}
}

func TestSourceResolutionRequiresInventoryAndUnknownIdentity(t *testing.T) {
	for _, inventory := range []bool{false, true} {
		t.Run(map[bool]string{false: "not inventory", true: "existing empty object"}[inventory], func(t *testing.T) {
			f := newFixture(t, 1)
			f.job.Source.Kind = "S3"
			if !inventory {
				f.job.Items[0].Source = transferstore.Object{Key: f.job.Items[0].Source.Key}
			}
			if err := os.WriteFile(filepath.Join(f.src, f.job.Items[0].Source.Key), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			f.startEngine(nil, Options{})
			if _, err := f.engine.Accept(bg, f.job, inventory); err != nil {
				t.Fatal(err)
			}
			if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
				t.Fatal(err)
			}
			done := f.wait()
			if done.Status != Failed || done.Results[0].ResolvedSource != nil || !strings.Contains(done.Results[0].Error, "source changed") {
				t.Fatalf("identity protection bypassed: %+v", done)
			}
		})
	}
}

func TestResolutionValidationRejectsUnrelatedOrChangedPins(t *testing.T) {
	item := Item{ID: "i", Source: transferstore.Object{Key: "missing"}}
	pin := transferstore.Object{Key: "missing", Size: 23, ETag: "identity"}
	previous := Record{Job: Job{Source: Endpoint{Kind: "S3"}, Items: []Item{item}}, Results: []ItemResult{{ID: "i", ResolvedSource: &pin}}}
	for _, name := range []string{"noninventory", "known source", "changed pin", "removed pin", "wrong key", "empty identity"} {
		t.Run(name, func(t *testing.T) {
			current := previous
			current.Job.Items = append([]Item(nil), previous.Job.Items...)
			current.Results = append([]ItemResult(nil), previous.Results...)
			changed := pin
			current.Results[0].ResolvedSource = &changed
			inventory := true
			switch name {
			case "noninventory":
				inventory = false
			case "known source":
				current.Job.Items[0].Source.ETag = "old"
			case "changed pin":
				changed.ETag = "changed"
			case "removed pin":
				current.Results[0].ResolvedSource = nil
			case "wrong key":
				changed.Key = "other"
			case "empty identity":
				changed.ETag = ""
			}
			if err := current.ValidateResolvedSources(inventory, &previous); err == nil {
				t.Fatal("invalid pin accepted")
			}
		})
	}
}

func TestResolutionCommitFailureNeverOpensOrPublishesSource(t *testing.T) {
	f := missingInventoryFixture(t)
	f.job.Items = f.job.Items[1:]
	key := f.job.Items[0].Source.Key
	if err := os.WriteFile(filepath.Join(f.src, key), []byte("not yet pinned"), 0600); err != nil {
		t.Fatal(err)
	}
	observed := &observedDB{db: f.db, child: f.job.ChildID}
	var err error
	f.repo, err = NewRepository(observed, RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	if _, err = f.engine.Accept(bg, f.job, true); err != nil {
		t.Fatal(err)
	}
	record, err := f.repo.begin(bg, f.job.ChildID, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	src, dst, err := f.factory().Open(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	defer dst.Close()
	var opens atomic.Int64
	source := &pinCheckingSource{Store: src, repo: f.repo, child: f.job.ChildID, opens: &opens}
	var beforeFormat struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := f.db.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &beforeFormat); return err }); err != nil {
		t.Fatal(err)
	}
	observed.failAll.Store(true)
	result := f.engine.copyResolvedItem(bg, record, f.job.Items[0], source, dst)
	observed.failAll.Store(false)
	if result.Status != ItemFailed || !strings.Contains(result.Error, "injected durable commit failure") || opens.Load() != 0 {
		t.Fatalf("read before successful durable pin: %+v opens=%d", result, opens.Load())
	}
	saved, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	var format struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := f.db.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &format); return err }); err != nil {
		t.Fatal(err)
	}
	if format != beforeFormat {
		t.Fatalf("failed pin upgraded format: %+v", format)
	}
	if saved.Results[0].ResolvedSource != nil || saved.Counters.TotalBytes != 0 {
		t.Fatalf("failed pin transaction committed: %+v", saved)
	}
	if _, err = os.Stat(filepath.Join(f.dst, f.job.Items[0].DestinationKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed pin published destination: %v", err)
	}
}
